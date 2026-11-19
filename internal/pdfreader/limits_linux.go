//go:build linux && (amd64 || arm64)

package pdfreader

import (
	"fmt"
	"os/signal"
	"runtime/debug"
	"syscall"
	"unsafe"
)

// processLimits is what the command imposes on itself before it reads a byte of the document (ADR-0001 D31). They are
// the operating system's: the Go runtime's own soft memory limit (GOMEMLIMIT, debug.SetMemoryLimit) only makes the
// collector work harder and never refuses an allocation, so it is set too but is not the control.
type processLimits struct {
	MemoryBytes  uint64 // RLIMIT_DATA: memory the process may hold; an allocation beyond it fails and the runtime aborts the process
	AddressBytes uint64 // RLIMIT_AS: address space, for what RLIMIT_DATA does not see (the Go runtime reserves a lot without using it)
	CPUSeconds   uint64 // RLIMIT_CPU: the kernel ends a process that spins past it, whatever its own timer does
}

func applyProcessLimits(l processLimits) error {
	// A write beyond RLIMIT_FSIZE (0, so any write to a regular file) raises SIGXFSZ, whose default action ends the
	// process without a word. The Go runtime does not act on it; it is ignored here as well so that the write returns
	// EFBIG, which the command turns into a message, whatever the program that embeds it has set up.
	signal.Ignore(syscall.SIGXFSZ)
	set := func(res int, v uint64) error {
		return syscall.Setrlimit(res, &syscall.Rlimit{Cur: v, Max: v})
	}
	// No file can be written (RLIMIT_FSIZE), no core dump, and no more descriptors than standard input and output, the
	// error stream and what the runtime opens for its poller. Nothing is read from the file system either: the
	// process opens nothing.
	for _, r := range []struct {
		res  int
		v    uint64
		name string
	}{
		{syscall.RLIMIT_AS, l.AddressBytes, "RLIMIT_AS"},
		{syscall.RLIMIT_DATA, l.MemoryBytes, "RLIMIT_DATA"},
		{syscall.RLIMIT_CPU, l.CPUSeconds, "RLIMIT_CPU"},
		{syscall.RLIMIT_FSIZE, 0, "RLIMIT_FSIZE"},
		{syscall.RLIMIT_CORE, 0, "RLIMIT_CORE"},
		{syscall.RLIMIT_NOFILE, 16, "RLIMIT_NOFILE"},
	} {
		if err := set(r.res, r.v); err != nil {
			return fmt.Errorf("%s: %w", r.name, err)
		}
	}
	// A third of the limit as the soft target, so that the collector runs before the hard limit is near.
	debug.SetMemoryLimit(int64(l.MemoryBytes / 3 * 2))
	return applySeccomp()
}

// The system calls the command never makes and that a hostile document must not be able to cause through a parser bug:
// making a connection, listening, starting a program, tracing. They fail with EPERM; every other call is allowed (the
// runtime needs many, and a longer list would need a table per architecture and release).
var denied = []uint32{
	syscall.SYS_SOCKET, syscall.SYS_CONNECT, syscall.SYS_BIND, syscall.SYS_LISTEN, syscall.SYS_ACCEPT4,
	syscall.SYS_EXECVE, sysExecveat, syscall.SYS_PTRACE,
}

type sockFilter struct {
	code uint16
	jt   uint8
	jf   uint8
	k    uint32
}

type sockFprog struct {
	length uint16
	_      [6]byte
	filter *sockFilter
}

const (
	bpfLD                = 0x00
	bpfW                 = 0x00
	bpfABS               = 0x20
	bpfJMP               = 0x05
	bpfJEQ               = 0x10
	bpfK                 = 0x00
	bpfRET               = 0x06
	seccompRetAllow      = 0x7fff0000
	seccompRetErrno      = 0x00050000
	seccompSetModeFilter = 1
	seccompFilterTsync   = 1
	prSetNoNewPrivs      = 38
)

func applySeccomp() error {
	var prog []sockFilter
	ld := func(off uint32) sockFilter { return sockFilter{code: bpfLD | bpfW | bpfABS, k: off} }
	prog = append(prog,
		ld(4), // seccomp_data.arch
		sockFilter{code: bpfJMP | bpfJEQ | bpfK, jt: 1, k: auditArch},
		sockFilter{code: bpfRET | bpfK, k: seccompRetErrno | uint32(syscall.EPERM)}, // another architecture: nothing is trusted
		ld(0), // seccomp_data.nr
	)
	for i, nr := range denied {
		left := uint8(len(denied) - i - 1)
		prog = append(prog, sockFilter{code: bpfJMP | bpfJEQ | bpfK, jt: left + 1, k: nr})
	}
	prog = append(prog,
		sockFilter{code: bpfRET | bpfK, k: seccompRetAllow},
		sockFilter{code: bpfRET | bpfK, k: seccompRetErrno | uint32(syscall.EPERM)},
	)
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); e != 0 {
		return fmt.Errorf("no_new_privs: %w", e)
	}
	fp := sockFprog{length: uint16(len(prog)), filter: &prog[0]}
	if _, _, e := syscall.RawSyscall(sysSeccomp, seccompSetModeFilter, seccompFilterTsync, uintptr(unsafe.Pointer(&fp))); e != 0 {
		return fmt.Errorf("seccomp: %w", e)
	}
	return nil
}

// closeInheritedFiles closes every descriptor above the three standard ones. os/exec starts a process with only the
// descriptors the parent marked close-on-exec closed; the parent is the service, which opens files and sockets in
// libraries that may not have. close_range(2) is on Linux 5.9 and later; on an older kernel the loop closes the first
// few, which is where a leaked descriptor would be.
func closeInheritedFiles() {
	const sysCloseRange = 436 // the same number on amd64 and arm64
	if _, _, e := syscall.RawSyscall(sysCloseRange, 3, ^uintptr(0), 0); e == 0 {
		return
	}
	for fd := 3; fd < 256; fd++ {
		syscall.Close(fd)
	}
}
