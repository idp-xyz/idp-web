//go:build !(linux && (amd64 || arm64))

package pdfreader

import "errors"

type processLimits struct {
	MemoryBytes  uint64
	AddressBytes uint64
	CPUSeconds   uint64
}

// Where the limits cannot be applied the reader does not run: a parser of hostile files without its limits is not
// offered (ADR-0001 D31). The service then says that reading PDFs is not available.
func applyProcessLimits(processLimits) error {
	return errors.New("process limits are applied on linux/amd64 and linux/arm64 only")
}

func closeInheritedFiles() {}
