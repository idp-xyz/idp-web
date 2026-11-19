package deploy

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"idp-web/internal/addr"
	"idp-web/internal/tenants"
)

// Nobody may use the tools until an operator says so (appendix C 4.7): the shipped tenants file lists nobody, and its
// default, which every unlisted tenant gets, is off.
func TestShippedTenantsFileEnablesNobody(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(dir, "tenants.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	set, err := tenants.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if set.Count() != 0 {
		t.Errorf("the shipped file lists %d tenants", set.Count())
	}
	for _, id := range []string{"", "default", "acme", "7b0c0a52-0000-0000-0000-000000000000"} {
		if p := set.For(id); p.Enabled || p.OpenURL {
			t.Errorf("tenant %q gets %+v", id, p)
		}
	}
	var raw struct {
		Default map[string]any `yaml:"default"`
	}
	if err := yaml.Unmarshal(b, &raw); err != nil || raw.Default == nil || raw.Default["enabled"] != false {
		t.Errorf("the default is not stated as `enabled: false`: %v (%v)", raw.Default, err)
	}
}

func TestEgressPolicyIsTheOnlyWayOutAndIsIPv4Only(t *testing.T) {
	m := loadAll(t)
	var egress []doc
	for _, p := range m.byKind["NetworkPolicy"] {
		types := strs(list(p, "spec", "policyTypes"))
		for _, ty := range types {
			if ty == "Egress" && len(list(p, "spec", "egress")) > 0 {
				egress = append(egress, p)
			}
		}
	}
	// allow-dns (kube-dns, port 53), web-egress (the public internet) and web-database (PostgreSQL, tested by
	// TestTheDatabaseIsTheOnlyPlaceInTheClusterTheServiceAndTheJobMayReach): nothing else
	names := map[string]bool{}
	for _, p := range egress {
		names[str(p, "metadata", "name")] = true
	}
	if len(egress) != 3 || !names["allow-dns"] || !names["web-egress"] || !names["web-database"] {
		t.Fatalf("policies that allow egress: %v, want allow-dns, web-egress and web-database", names)
	}
	var web doc
	for _, p := range egress {
		if str(p, "metadata", "name") == "web-egress" {
			web = p
		}
	}
	if web == nil {
		t.Fatal("no web-egress")
	}
	sel, _ := get(web, "spec", "podSelector", "matchLabels").(doc)
	if len(sel) != 1 || sel["app.kubernetes.io/component"] != "web" {
		t.Errorf("web-egress selects %v", sel)
	}
	eg := list(web, "spec", "egress")
	if len(eg) != 1 || len(list(eg[0], "to")) != 1 {
		t.Fatalf("egress %v", eg)
	}
	to := list(eg[0], "to")[0]
	if get(to, "namespaceSelector") != nil || get(to, "podSelector") != nil || str(to, "ipBlock", "cidr") != "0.0.0.0/0" {
		t.Fatalf("egress peer %v: IPv4 addresses outside the cluster only (an IPv6 rule needs its own excepts)", to)
	}
	var except []netip.Prefix
	for _, c := range list(to, "ipBlock", "except") {
		p := netip.MustParsePrefix(c.(string))
		if !p.Addr().Is4() {
			t.Errorf("except %s is not IPv4", p)
		}
		except = append(except, p)
	}
	excluded := func(a netip.Addr) bool {
		for _, e := range except {
			if e.Contains(a) {
				return true
			}
		}
		return false
	}
	// What the service refuses to read from, the cluster refuses too, and nothing else: every /24 of the IPv4 space is
	// compared (the smallest block of the table is a /24, and 192.0.0.0/24 is refused whole), so a block added to the code's
	// table and not here is found, and so is an except that cuts off public addresses.
	for n := uint64(0); n < 1<<32; n += 256 {
		a := netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), 0})
		if refused := !addr.Public(a); refused != excluded(a) {
			t.Fatalf("%s: the service refuses it: %v; the policy excludes it: %v", a, refused, excluded(a))
		}
	}
	for _, a := range []string{"0.0.0.0", "10.255.255.254", "100.127.255.254", "127.0.0.1", "169.254.169.254", "172.31.255.254", "192.0.0.9", "192.0.0.10",
		"192.168.255.254", "198.19.255.254", "239.255.255.250", "240.0.0.1", "255.255.255.255"} {
		if a := netip.MustParseAddr(a); !excluded(a) || addr.Public(a) {
			t.Errorf("%s: excluded %v, public per the code %v", a, excluded(a), addr.Public(a))
		}
	}
	for _, a := range []string{"1.1.1.1", "93.184.216.34", "151.101.0.223", "192.31.196.1"} {
		if excluded(netip.MustParseAddr(a)) {
			t.Errorf("the public address %s is excluded", a)
		}
	}
	ports := list(eg[0], "ports")
	if len(ports) != 2 {
		t.Fatalf("ports %v", ports)
	}
	for _, p := range ports {
		if pv := get(p, "port"); (pv != 443 && pv != 80) || str(p, "protocol") != "TCP" {
			t.Errorf("egress port %v is not 443 or 80", p)
		}
	}
}

func TestIngressAdmitsTheGatewayAndTheRegistryOnTheMCPPortAndNobodyElse(t *testing.T) {
	m := loadAll(t)
	var found doc
	for _, p := range m.byKind["NetworkPolicy"] {
		if str(p, "metadata", "name") == "web-ingress" {
			found = p
		}
	}
	if found == nil {
		t.Fatal("no web-ingress")
	}
	rules := list(found, "spec", "ingress")
	if len(rules) != 2 {
		t.Fatalf("ingress %v", rules)
	}
	seen := map[string]bool{}
	for _, f := range list(rules[0], "from") {
		nsSel, _ := get(f, "namespaceSelector", "matchLabels").(doc)
		podSel, _ := get(f, "podSelector", "matchLabels").(doc)
		if len(nsSel) != 1 || nsSel["kubernetes.io/metadata.name"] != "idp-mcp" || len(podSel) != 1 || get(f, "ipBlock") != nil {
			t.Errorf("peer %v", f)
		}
		seen[podSel["app.kubernetes.io/component"].(string)] = true
	}
	if !seen["gateway"] || !seen["registry"] || len(seen) != 2 {
		t.Errorf("peers %v", seen)
	}
	if p := list(rules[0], "ports"); len(p) != 1 || get(p[0], "port") != 8580 {
		t.Errorf("ports %v", p)
	}
	// the ops port is for the namespace that scrapes metrics, not the gateway
	if p := list(rules[1], "ports"); len(p) != 1 || get(p[0], "port") != 9580 {
		t.Errorf("ops ports %v", p)
	}
	for _, f := range list(rules[1], "from") {
		if nsSel, _ := get(f, "namespaceSelector", "matchLabels").(doc); nsSel["kubernetes.io/metadata.name"] != "observability" {
			t.Errorf("ops peer %v", f)
		}
	}
	// default deny stays
	var deny bool
	for _, p := range m.byKind["NetworkPolicy"] {
		if str(p, "metadata", "name") == "default-deny" && len(get(p, "spec", "podSelector").(doc)) == 0 && len(strs(list(p, "spec", "policyTypes"))) == 2 &&
			get(p, "spec", "ingress") == nil && get(p, "spec", "egress") == nil {
			deny = true
		}
	}
	if !deny {
		t.Error("no default-deny of both directions")
	}
}
