package main

import (
	"bufio"
	"crypto/sha1"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// Hashing helpers (v1 pieces and v2 merkle trees)

type pieceHasher struct {
	h        hash.Hash
	pieceLen int64
	inPiece  int64
	out      []byte
}

func newPieceHasher(pieceLen int64) *pieceHasher {
	return &pieceHasher{h: sha1.New(), pieceLen: pieceLen}
}

func (p *pieceHasher) Write(data []byte) (int, error) {
	total := len(data)
	for len(data) > 0 {
		rem := p.pieceLen - p.inPiece
		n := int64(len(data))
		if n > rem {
			n = rem
		}
		_, _ = p.h.Write(data[:n])
		p.inPiece += n
		data = data[n:]
		if p.inPiece == p.pieceLen {
			p.finishPiece()
		}
	}
	return total, nil
}

func (p *pieceHasher) finishPiece() {
	p.out = append(p.out, p.h.Sum(nil)...)
	p.h.Reset()
	p.inPiece = 0
}

func (p *pieceHasher) WriteZeros(n int64) {
	if n <= 0 {
		return
	}
	zeros := make([]byte, 64*1024)
	for n > 0 {
		chunk := int64(len(zeros))
		if n < chunk {
			chunk = n
		}
		_, _ = p.Write(zeros[:chunk])
		n -= chunk
	}
}

func (p *pieceHasher) Finish() []byte {
	if p.inPiece > 0 {
		p.finishPiece()
	}
	return p.out
}

func nextPow2(n int) int {
	if n <= 1 {
		return 1
	}
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

func merkleRoot(hashes [][sha256.Size]byte) [sha256.Size]byte {
	if len(hashes) == 0 {
		return [sha256.Size]byte{}
	}
	level := make([][sha256.Size]byte, len(hashes))
	copy(level, hashes)
	var pair [2 * sha256.Size]byte
	for len(level) > 1 {
		for i := 0; i < len(level); i += 2 {
			copy(pair[:sha256.Size], level[i][:])
			copy(pair[sha256.Size:], level[i+1][:])
			level[i/2] = sha256.Sum256(pair[:])
		}
		level = level[:len(level)/2]
	}
	return level[0]
}

func v2PadHash(pieceLen int64) [sha256.Size]byte {
	blocks := int(pieceLen / blockSize)
	zeros := make([][sha256.Size]byte, blocks)
	return merkleRoot(zeros)
}

type v2Hashes struct {
	Root       [sha256.Size]byte
	HasRoot    bool
	Layer      []byte
	PieceRoots [][sha256.Size]byte
	Length     int64
}

func v2FileHashes(path string, pieceLen int64) (v2Hashes, error) {
	var out v2Hashes
	if pieceLen < blockSize || pieceLen%blockSize != 0 || pieceLen&(pieceLen-1) != 0 {
		return out, fmt.Errorf("invalid v2 piece length %d", pieceLen)
	}
	f, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1024*1024)
	blocksPerPiece := int(pieceLen / blockSize)
	buf := make([]byte, blockSize)
	var padHash [sha256.Size]byte
	padHashReady := false
	eof := false
	for !eof {
		blocks := make([][sha256.Size]byte, 0, blocksPerPiece)
		for j := 0; j < blocksPerPiece; j++ {
			n, err := io.ReadFull(r, buf)
			if n > 0 {
				blocks = append(blocks, sha256.Sum256(buf[:n]))
				out.Length += int64(n)
			}
			if err == io.EOF {
				eof = true
				break
			}
			if err == io.ErrUnexpectedEOF {
				eof = true
				break
			}
			if err != nil {
				return out, err
			}
		}
		if len(blocks) == 0 {
			break
		}
		required := blocksPerPiece
		if len(out.PieceRoots) == 0 {
			required = nextPow2(len(blocks))
		}
		if required < len(blocks) {
			required = len(blocks)
		}
		for len(blocks) < required {
			blocks = append(blocks, [sha256.Size]byte{})
		}
		out.PieceRoots = append(out.PieceRoots, merkleRoot(blocks))
		if eof {
			break
		}
	}
	if len(out.PieceRoots) == 0 {
		return out, nil
	}
	if len(out.PieceRoots) == 1 {
		out.Root = out.PieceRoots[0]
		out.HasRoot = true
		return out, nil
	}
	if !padHashReady {
		padHash = v2PadHash(pieceLen)
		padHashReady = true
	}
	for _, pr := range out.PieceRoots {
		out.Layer = append(out.Layer, pr[:]...)
	}
	target := nextPow2(len(out.PieceRoots))
	padded := make([][sha256.Size]byte, target)
	copy(padded, out.PieceRoots)
	for i := len(out.PieceRoots); i < target; i++ {
		padded[i] = padHash
	}
	out.Root = merkleRoot(padded)
	out.HasRoot = true
	return out, nil
}

func feedFileExact(ph *pieceHasher, path string, length int64) error {
	if length == 0 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() != length {
		return fmt.Errorf("%s: size is %d, expected %d", path, st.Size(), length)
	}
	n, err := io.CopyN(ph, f, length)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if n != length {
		return fmt.Errorf("%s: read %d bytes, expected %d", path, n, length)
	}
	return nil
}

// feedFileForVerify always feeds exactly length bytes to the hasher. Missing
// or short files are zero-filled so that later piece indices stay aligned.
func feedFileForVerify(ph *pieceHasher, path string, length int64) error {
	if length == 0 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		ph.WriteZeros(length)
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		ph.WriteZeros(length)
		return err
	}
	var problems []string
	if st.Size() != length {
		problems = append(problems, fmt.Sprintf("size is %d, expected %d", st.Size(), length))
	}
	n, copyErr := io.CopyN(ph, f, length)
	if n < length {
		ph.WriteZeros(length - n)
	}
	if copyErr != nil && copyErr != io.EOF {
		problems = append(problems, copyErr.Error())
	} else if n < length && st.Size() == length {
		problems = append(problems, fmt.Sprintf("short read: %d of %d bytes", n, length))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s: %s", path, strings.Join(problems, "; "))
	}
	return nil
}

func safeJoinComponents(base string, comps []string) (string, error) {
	full := base
	for _, c := range comps {
		if c == "" || c == "." || c == ".." || strings.ContainsAny(c, "/\\\x00") {
			return "", fmt.Errorf("unsafe path component %q", cleanText([]byte(c)))
		}
		full = filepath.Join(full, c)
	}
	return full, nil
}

func v1DataPath(a *torrentAnalysis, dataDir string, f fileEntry) (string, error) {
	if a.SingleFile {
		return safeJoinComponents(dataDir, []string{string(a.NameRaw)})
	}
	return safeJoinComponents(dataDir, append([]string{string(a.NameRaw)}, f.Path...))
}

func v2DataPath(a *torrentAnalysis, dataDir string, f fileEntry) (string, error) {
	if a.SingleFile {
		comps := f.Path
		if len(comps) == 0 {
			comps = []string{string(a.NameRaw)}
		}
		return safeJoinComponents(dataDir, comps)
	}
	return safeJoinComponents(dataDir, append([]string{string(a.NameRaw)}, f.Path...))
}
