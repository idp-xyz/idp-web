// Package deploy checks deploy/kubernetes for internal consistency and against the code, with no cluster involved: every
// object is named and namespaced, the pod is hardened, the mounts, ports and settings agree with each other and with what
// the service reads, nothing test-only is shipped, the shipped tenants file enables nobody, and the egress policy excludes
// exactly the IPv4 addresses the service refuses to read from (egress_test.go). It cannot show that a CNI enforces the
// policies: that is the platform's kind run (PATH M and N).
package deploy

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	dir = "../../deploy/kubernetes"
	ns  = "idp-web"
)

type doc = map[string]any

func load(t *testing.T, name string) []doc {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	var out []doc
	for {
		var d doc
		if err := dec.Decode(&d); errors.Is(err, io.EOF) {
			return out
		} else if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if d != nil {
			out = append(out, d)
		}
	}
}

func get(d any, path ...string) any {
	for _, p := range path {
		m, ok := d.(doc)
		if !ok {
			return nil
		}
		d = m[p]
	}
	return d
}

func str(d any, path ...string) string { s, _ := get(d, path...).(string); return s }
func list(d any, path ...string) []any { l, _ := get(d, path...).([]any); return l }
func name(d doc) string                { return str(d, "kind") + "/" + str(d, "metadata", "name") }

func strs(l []any) []string {
	var out []string
	for _, v := range l {
		out = append(out, v.(string))
	}
	sort.Strings(out)
	return out
}

type manifests struct {
	kust   doc
	docs   []doc
	byKind map[string][]doc
}

func loadAll(t *testing.T) *manifests {
	t.Helper()
	m := &manifests{kust: load(t, "kustomization.yaml")[0], byKind: map[string][]doc{}}
	for _, r := range list(m.kust, "resources") {
		for _, d := range load(t, r.(string)) {
			m.docs = append(m.docs, d)
			m.byKind[str(d, "kind")] = append(m.byKind[str(d, "kind")], d)
		}
	}
	return m
}

func (m *manifests) one(t *testing.T, kind string) doc {
	t.Helper()
	if len(m.byKind[kind]) != 1 {
		t.Fatalf("%d %s, want one", len(m.byKind[kind]), kind)
	}
	return m.byKind[kind][0]
}

func envOf(container any) map[string]string {
	out := map[string]string{}
	for _, e := range list(container, "env") {
		out[str(e, "name")] = str(e, "value")
	}
	return out
}

func TestKustomizationListsEveryFileAndUsesLabelsNotCommonLabels(t *testing.T) {
	m := loadAll(t)
	if m.kust["commonLabels"] != nil || m.kust["namespace"] != nil {
		t.Error("commonLabels rewrites NetworkPolicy peer selectors (the gateway, kube-dns), which then match nothing")
	}
	if ls := list(m.kust, "labels"); len(ls) != 1 || get(ls[0], "includeTemplates") != true || get(ls[0], "includeSelectors") != nil {
		t.Errorf("labels: %v", ls)
	}
	listed := map[string]bool{"kustomization.yaml": true}
	for _, r := range list(m.kust, "resources") {
		listed[r.(string)] = true
	}
	for _, g := range list(m.kust, "configMapGenerator") {
		for _, f := range list(g, "files") {
			listed[f.(string)] = true
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !listed[e.Name()] {
			t.Errorf("%s is not used by the kustomization", e.Name())
		}
		if n := strings.ToLower(e.Name()); strings.Contains(n, "key") || strings.Contains(n, "secret") {
			t.Errorf("%s: a key or secret does not belong in the repository", e.Name())
		}
	}
	imgs := list(m.kust, "images")
	if len(imgs) != 1 || str(imgs[0], "name") != "idp-web" || !strings.HasPrefix(str(imgs[0], "digest"), "sha256:") ||
		!strings.HasPrefix(str(imgs[0], "newName"), "registry.example.com/idp/") {
		t.Errorf("the image is not a placeholder pinned by digest: %v", imgs)
	}
}

func TestEveryManifestIsNamedAndInTheNamespace(t *testing.T) {
	m := loadAll(t)
	for _, d := range m.docs {
		if str(d, "apiVersion") == "" || str(d, "kind") == "" || str(d, "metadata", "name") == "" {
			t.Errorf("incomplete object: %v", d)
		}
		switch k := str(d, "kind"); k {
		case "Namespace":
			if str(d, "metadata", "name") != ns {
				t.Errorf("namespace %s", name(d))
			}
		case "Secret", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding", "ServiceAccount":
			t.Errorf("%s does not belong here: the operator creates the secrets, and the service has no credential for the API", name(d))
		default:
			if got := str(d, "metadata", "namespace"); got != ns {
				t.Errorf("%s: namespace %q", name(d), got)
			}
		}
	}
	for _, g := range list(m.kust, "configMapGenerator") {
		if str(g, "namespace") != ns {
			t.Errorf("generated ConfigMap %s is in namespace %q", str(g, "name"), str(g, "namespace"))
		}
	}
}

func TestNamespaceIsRestrictedAndOpenToTheGateway(t *testing.T) {
	n := loadAll(t).one(t, "Namespace")
	l, _ := get(n, "metadata", "labels").(doc)
	if l["pod-security.kubernetes.io/enforce"] != "restricted" || l["idp-mcp/mcp-upstream"] != "true" {
		t.Errorf("labels %v", l)
	}
}

// the service as the code reads it (cmd/idp-web/main.go and config.go)
func TestServiceIsHardenedAndItsSettingsAgreeWithItsMounts(t *testing.T) {
	m := loadAll(t)
	dep, svc := m.one(t, "Deployment"), m.one(t, "Service")
	if get(dep, "spec", "replicas") != 1 || str(dep, "spec", "strategy", "type") != "Recreate" {
		t.Error("phase 1 is one replica: numbering, cache and limits are per replica (ADR-0001 D19, D21, D22)")
	}
	pod := get(dep, "spec", "template", "spec")
	if get(pod, "automountServiceAccountToken") != false || get(pod, "enableServiceLinks") != false || get(pod, "hostNetwork") != nil {
		t.Error("the pod mounts a service account token, or has service links or the host network")
	}
	if get(pod, "securityContext", "runAsNonRoot") != true || str(pod, "securityContext", "seccompProfile", "type") != "RuntimeDefault" {
		t.Errorf("pod security context %v", get(pod, "securityContext"))
	}
	cs := list(pod, "containers")
	if len(cs) != 1 {
		t.Fatalf("%d containers", len(cs))
	}
	c := cs[0]
	sc := get(c, "securityContext")
	if get(sc, "allowPrivilegeEscalation") != false || get(sc, "readOnlyRootFilesystem") != true || strings.Join(strs(list(sc, "capabilities", "drop")), ",") != "ALL" ||
		get(c, "resources", "limits", "memory") == nil || get(c, "resources", "limits", "cpu") == nil ||
		get(c, "resources", "requests", "memory") == nil || get(c, "resources", "requests", "cpu") == nil {
		t.Errorf("container %v", c)
	}
	if str(c, "image") != "idp-web" {
		t.Errorf("image %q is not the one the kustomization remaps", str(c, "image"))
	}
	// ports: the service listens where the Service and the probes say
	env := envOf(c)
	ports := map[string]int{}
	for _, p := range list(c, "ports") {
		ports[str(p, "name")], _ = get(p, "containerPort").(int)
	}
	if ports["mcp"] != 8580 || ports["ops"] != 9580 || env["IDP_WEB_LISTEN"] != "0.0.0.0:8580" || env["IDP_WEB_METRICS"] != "0.0.0.0:9580" {
		t.Errorf("ports %v, listen %q, metrics %q", ports, env["IDP_WEB_LISTEN"], env["IDP_WEB_METRICS"])
	}
	if env["IDP_WEB_PRIVATE_NETWORK"] != "true" {
		t.Error("a non-loopback address needs IDP_WEB_PRIVATE_NETWORK=true")
	}
	if _, off := env["IDP_WEB_REQUIRE_CALLER"]; off {
		t.Error("IDP_WEB_REQUIRE_CALLER is on unless set; the shipped pod does not set it")
	}
	for probe, path := range map[string]string{"readinessProbe": "/readyz", "livenessProbe": "/healthz"} {
		if str(c, probe, "httpGet", "path") != path || str(c, probe, "httpGet", "port") != "ops" {
			t.Errorf("%s %v is not %s on the ops port", probe, get(c, probe), path)
		}
	}
	sp := list(svc, "spec", "ports")
	if len(sp) != 1 || get(sp[0], "port") != 8580 || str(sp[0], "targetPort") != "mcp" {
		t.Errorf("service ports %v", sp)
	}
	labels, _ := get(dep, "spec", "template", "metadata", "labels").(doc)
	sel, _ := get(svc, "spec", "selector").(doc)
	dsel, _ := get(dep, "spec", "selector", "matchLabels").(doc)
	if len(sel) != 1 || sel["app.kubernetes.io/component"] != "web" || labels["app.kubernetes.io/component"] != "web" || dsel["app.kubernetes.io/component"] != "web" {
		t.Errorf("selectors: service %v, deployment %v, pod labels %v", sel, dsel, labels)
	}
	// every file the service reads is in a read-only mount of a volume that exists
	mounts := map[string]string{}
	for _, mt := range list(c, "volumeMounts") {
		if get(mt, "readOnly") != true {
			t.Errorf("mount %v is writable", mt)
		}
		mounts[str(mt, "name")] = str(mt, "mountPath")
	}
	vols := map[string]doc{}
	for _, v := range list(pod, "volumes") {
		vols[str(v, "name")] = v.(doc)
	}
	if len(vols) != len(mounts) {
		t.Errorf("%d volumes, %d mounts", len(vols), len(mounts))
	}
	for n := range mounts {
		if vols[n] == nil {
			t.Errorf("mount %s has no volume", n)
		}
	}
	files := map[string]struct{ volume, key string }{
		"IDP_WEB_REF_KEY_FILE":     {"ref-key", "ref.key"},
		"IDP_WEB_CALLER_KEYS_FILE": {"gateway-caller-keys", "caller.pub"},
		"IDP_WEB_TENANTS_FILE":     {"tenants", "tenants.yaml"},
	}
	for v, want := range files {
		p := env[v]
		if p != mounts[want.volume]+"/"+want.key {
			t.Errorf("%s=%q is not %s/%s", v, p, mounts[want.volume], want.key)
		}
	}
	// the secrets and the public key are the operator's: named, not shipped; the tenants file is the one the kustomization generates
	if str(vols["ref-key"], "secret", "secretName") != "idp-web-ref-key" || str(vols["gateway-caller-keys"], "configMap", "name") != "idp-web-gateway-caller-keys" {
		t.Errorf("volumes %v", vols)
	}
	if str(vols["search-key"], "secret", "secretName") != "idp-web-search-key" || get(vols["search-key"], "secret", "optional") != true {
		t.Errorf("the search key's Secret is optional (no provider is chosen as shipped) and named idp-web-search-key: %v", vols["search-key"])
	}
	gen := list(m.kust, "configMapGenerator")
	if len(gen) != 1 || str(gen[0], "name") != str(vols["tenants"], "configMap", "name") || len(list(gen[0], "files")) != 1 || list(gen[0], "files")[0] != "tenants.yaml" {
		t.Errorf("the tenants ConfigMap %q is not the one the kustomization generates: %v", str(vols["tenants"], "configMap", "name"), gen)
	}
	// the audience is the Service's URL as it is registered at the gateway: the form the README and the RUNBOOK document
	want := "http://" + str(svc, "metadata", "name") + "." + ns + ".svc.cluster.local:8580/mcp"
	if env["IDP_WEB_CALLER_AUDIENCE"] != want {
		t.Errorf("IDP_WEB_CALLER_AUDIENCE=%q, the Service's URL is %q", env["IDP_WEB_CALLER_AUDIENCE"], want)
	}
	for _, f := range []string{"../../README.md", "../../docs/RUNBOOK.md"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), want) {
			t.Errorf("%s does not document the audience %s", f, want)
		}
	}
}

// Plan 4.4 and ADR-0001 D14: the exemption for test origins is for tests, and a manifest that ships it ships a hole.
func TestNothingTestOnlyIsShipped(t *testing.T) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		// not even named in a comment, except where the comment forbids it (web.yaml)
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "IDP_WEB_TEST_ORIGINS") && !(e.Name() == "web.yaml" && strings.Contains(line, "Never IDP_WEB_TEST_ORIGINS")) {
				t.Errorf("%s:%d mentions IDP_WEB_TEST_ORIGINS: %s", e.Name(), i+1, line)
			}
		}
	}
	m := loadAll(t)
	c := list(get(m.one(t, "Deployment"), "spec", "template", "spec"), "containers")[0]
	env := envOf(c)
	if _, ok := env["IDP_WEB_TEST_ORIGINS"]; ok {
		t.Error("IDP_WEB_TEST_ORIGINS is set")
	}
	if p := env["IDP_WEB_PROVIDER"]; p != "" {
		// the provider is the operator's choice (a vendor decides who sees every query) and the fixture is never production
		t.Errorf("IDP_WEB_PROVIDER=%q is shipped: it is left to the operator", p)
	}
	for k := range env {
		if strings.HasPrefix(k, "IDP_WEB_FIXTURE") {
			t.Errorf("%s is shipped", k)
		}
	}
	for _, d := range m.byKind["ConfigMap"] {
		t.Errorf("%s: the only ConfigMap is generated from tenants.yaml", name(d))
	}
}

var secretish = regexp.MustCompile(`(?i)(BEGIN [A-Z ]*(PRIVATE|PUBLIC) KEY|[A-Za-z0-9+/]{40,}={0,2}$)`)

func TestNoKeyMaterialInTheFiles(t *testing.T) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		for i, line := range strings.Split(string(b), "\n") {
			if secretish.MatchString(strings.TrimSpace(line)) && !strings.Contains(line, "sha256:") {
				t.Errorf("%s:%d looks like key material: %s", e.Name(), i+1, line)
			}
		}
	}
}
