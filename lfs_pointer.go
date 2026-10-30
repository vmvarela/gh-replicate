package main

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// lfsPrefixSize is how much of a blob Git LFS 3.8.1 parses (one read of up to
// 1024 bytes); anything after it is ignored there, so it is ignored here too.
const lfsPrefixSize = 1024

// lfsPointer identifies the object a confirmed pointer refers to.
type lfsPointer struct {
	OID  string // lowercase hex SHA-256
	Size int64
}

var lfsOIDRE = regexp.MustCompile(`\Asha256:[0-9a-f]{64}\z`)

// classifyLFSBlob inspects the first min(blobSize, 1024) bytes of a non-empty
// blob, passed as prefix. Results:
//   - none: no known version alias or recognizable Git LFS version line.
//   - confirmed: a complete, canonical v1 pointer with positive size.
//   - blocked: a candidate outside the supported subset; callers must stop.
//
// shortcut: noncanonical pointers remain blocked until their transfer is proven;
// unknown future formats cannot be detected universally. Callers skip empty blobs.
func classifyLFSBlob(prefix []byte, blobSize int64) (p lfsPointer, found bool, blocked string) {
	// Fail closed first: "none" is only valid for the complete committed prefix.
	if blobSize <= 0 || int64(len(prefix)) != min(blobSize, lfsPrefixSize) {
		return p, false, "prefix does not match min(blob size, 1024) bytes"
	}
	candidate := isLFSPointer(prefix)
	for _, line := range bytes.Split(prefix, []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("version https://git-lfs.github.com/spec/")) {
			candidate = true
		}
	}
	if !candidate {
		return lfsPointer{}, false, ""
	}
	if blobSize >= lfsPrefixSize {
		return p, false, "Git LFS pointer is too large for the supported format"
	}
	lines := strings.Split(string(prefix), "\n")
	if len(lines) != 4 || lines[0] != "version https://git-lfs.github.com/spec/v1" || lines[3] != "" {
		return p, false, "unsupported or noncanonical Git LFS pointer"
	}
	oid, ok := strings.CutPrefix(lines[1], "oid ")
	if !ok || !lfsOIDRE.MatchString(oid) {
		return p, false, "invalid pointer oid"
	}
	sizeText, ok := strings.CutPrefix(lines[2], "size ")
	size, err := strconv.ParseInt(sizeText, 10, 64)
	if !ok || err != nil || size <= 0 || strconv.FormatInt(size, 10) != sizeText {
		return p, false, "invalid or empty pointer size"
	}
	return lfsPointer{OID: strings.TrimPrefix(oid, "sha256:"), Size: size}, true, ""
}

// lfsInventory maps each OID to its size so conflicting pointers are caught.
type lfsInventory map[string]int64

func (inv lfsInventory) add(p lfsPointer) error {
	if old, ok := inv[p.OID]; ok && old != p.Size {
		return fmt.Errorf("conflicting Git LFS sizes for object %s: %d and %d", p.OID, old, p.Size)
	}
	inv[p.OID] = p.Size
	return nil
}
