package main

import (
	"bufio"
	"bytes"
	"errors"
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

// lfsVersionAliases are the three version URLs Git LFS 3.8.1 accepts.
var lfsVersionAliases = []string{
	"http://git-media.io/v/2",
	"https://hawser.github.com/spec/v1",
	"https://git-lfs.github.com/spec/v1",
}

// lfsExtRE matches an extension line key (ext-N-name), mirroring Git LFS 3.8.1.
var lfsExtRE = regexp.MustCompile(`\Aext-\d{1}-\w+\z`)

// lfsPointerKeys is the required key order Git LFS 3.8.1 enforces.
var lfsPointerKeys = [3]string{"version", "oid", "size"}

// classifyLFSBlob inspects the first min(blobSize, 1024) bytes of a non-empty
// blob, passed as prefix. Results:
//   - none: no known version alias or recognizable Git LFS version line, or a
//     size-0 pointer (empty object; git-lfs stores nothing for it).
//   - confirmed: a Git LFS pointer whose object the transfer path copies (any
//     3.8.1 alias, CRLF, surrounding whitespace, blank lines and extensions, a
//     positive canonical size, any blob length).
//   - blocked: a candidate outside the supported subset; callers must stop.
//
// It mirrors Git LFS 3.8.1 pointer.DecodeFrom/decodeKV. Oversized (>=1024
// bytes) padded pointers decode from their first 1024 bytes and transfer via a
// separate path, because the CLI scanner's DecodePointerFromBlob rejects them.
// Unknown future formats cannot be detected universally. Callers skip empty
// blobs.
func classifyLFSBlob(prefix []byte, blobSize int64) (p lfsPointer, found bool, blocked string) {
	// Fail closed first: "none" is only valid for the complete committed prefix.
	if blobSize <= 0 || int64(len(prefix)) != min(blobSize, lfsPrefixSize) {
		return p, false, "prefix does not match min(blob size, 1024) bytes"
	}
	if !isLFSPointer(prefix) && !hasFutureLFSVersion(prefix) {
		return lfsPointer{}, false, ""
	}
	p, err := decodeLFSKV(bytes.TrimSpace(prefix))
	if err != nil {
		return lfsPointer{}, false, err.Error()
	}
	if p.Size == 0 {
		// Empty object: git-lfs never stores one ("empty files are special").
		return lfsPointer{}, false, ""
	}
	return p, true, ""
}

// hasFutureLFSVersion detects a recognizable future Git LFS version line so a
// newer pointer blocks instead of being mistaken for ordinary content.
func hasFutureLFSVersion(prefix []byte) bool {
	for _, line := range bytes.Split(prefix, []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("version https://git-lfs.github.com/spec/")) {
			return true
		}
	}
	return false
}

// decodeLFSKV mirrors Git LFS 3.8.1 pointer.decodeKV over already-trimmed data:
// version/oid/size in order, extension lines allowed before any of them, a
// canonical positive size, and nothing after size. Extension values are not
// needed to transfer (the stored object is keyed by the main OID), so they are
// accepted by key only.
func decodeLFSKV(data []byte) (lfsPointer, error) {
	kvps := make(map[string]string, 3)
	line := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		text := sc.Text()
		if len(text) == 0 {
			continue
		}
		if len(lfsPointerKeys) <= line {
			return lfsPointer{}, errors.New("Git LFS pointer has content after size")
		}
		key, value, ok := strings.Cut(text, " ")
		if !ok {
			return lfsPointer{}, errors.New("Git LFS pointer line is missing a value")
		}
		if key != lfsPointerKeys[line] {
			if !lfsExtRE.MatchString(key) {
				return lfsPointer{}, fmt.Errorf("unsupported Git LFS pointer key %q", key)
			}
			continue // extension line; value is irrelevant to transfer
		}
		kvps[key] = value
		line++
	}
	if err := sc.Err(); err != nil {
		return lfsPointer{}, err
	}
	if !validLFSVersion(kvps["version"]) {
		return lfsPointer{}, errors.New("unsupported Git LFS version")
	}
	oid, ok := kvps["oid"]
	if !ok || !lfsOIDRE.MatchString(oid) {
		return lfsPointer{}, errors.New("invalid Git LFS pointer object id")
	}
	sizeText, ok := kvps["size"]
	size, err := strconv.ParseInt(sizeText, 10, 64)
	if !ok || err != nil || size < 0 || strconv.FormatInt(size, 10) != sizeText {
		return lfsPointer{}, errors.New("invalid Git LFS pointer size")
	}
	return lfsPointer{OID: strings.TrimPrefix(oid, "sha256:"), Size: size}, nil
}

func validLFSVersion(v string) bool {
	for _, a := range lfsVersionAliases {
		if v == a {
			return true
		}
	}
	return false
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
