package bundle

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	EncryptionName    = "AES-256-GCM-chunked-v2"
	DefaultChunkSize  = 1024 * 1024
	maxChunkSize      = 64 * 1024 * 1024
	frameFinal        = uint32(1 << 31)
	noncePrefixSize   = 12 // 96 bits of per-stream randomness
	headerSize        = 5 + 4 + noncePrefixSize
	maxCiphertextSize = maxChunkSize + 32
)

// streamMagic is part of the on-disk format. v2 deliberately rejects v1
// streams because v1 used only a 32-bit random nonce prefix and an
// unauthenticated end marker.
var (
	streamMagic       = [5]byte{'D', 'B', 'A', 'E', '2'}
	legacyStreamMagic = [5]byte{'D', 'B', 'A', 'E', '1'}
)

type EncryptOptions struct{ ChunkSize int }
type EncryptOption func(*EncryptOptions)

// StreamInfo describes the authenticated stream format and framing options.
// It is returned by DecryptWithInfo after a stream has been fully verified.
type StreamInfo struct {
	Version   int
	ChunkSize int
}

func WithChunkSize(n int) EncryptOption { return func(o *EncryptOptions) { o.ChunkSize = n } }

func validateKey(key []byte) error {
	if len(key) != 32 {
		return fmt.Errorf("encryption key must be 32 bytes, got %d", len(key))
	}
	return nil
}

func makeAEAD(key []byte) (cipher.AEAD, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func Encrypt(dst io.Writer, src io.Reader, key []byte, opts ...EncryptOption) error {
	if dst == nil || src == nil {
		return errors.New("source and destination are required")
	}
	aead, err := makeAEAD(key)
	if err != nil {
		return err
	}
	o := EncryptOptions{ChunkSize: DefaultChunkSize}
	for _, opt := range opts {
		opt(&o)
	}
	if o.ChunkSize < 4096 || o.ChunkSize > maxChunkSize {
		return fmt.Errorf("chunk size out of range: %d", o.ChunkSize)
	}
	noncePrefix := make([]byte, noncePrefixSize)
	if _, err := io.ReadFull(rand.Reader, noncePrefix); err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}
	header := make([]byte, headerSize)
	copy(header[:5], streamMagic[:])
	binary.BigEndian.PutUint32(header[5:9], uint32(o.ChunkSize))
	copy(header[9:], noncePrefix)
	if _, err := dst.Write(header); err != nil {
		return err
	}
	buf := make([]byte, o.ChunkSize)
	var index uint64
	for {
		n, readErr := io.ReadAtLeast(src, buf, 1)
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return readErr
		}
		if n > 0 {
			frameLen := uint32(n + aead.Overhead())
			frame := make([]byte, 4)
			binary.BigEndian.PutUint32(frame, frameLen)
			nonce := streamNonce(noncePrefix, index)
			sealed := aead.Seal(nil, nonce, buf[:n], frameAAD(header, index, frameLen))
			if _, err := dst.Write(frame); err != nil {
				return err
			}
			if _, err := dst.Write(sealed); err != nil {
				return err
			}
			index++
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
	}
	final := make([]byte, 4)
	binary.BigEndian.PutUint32(final, frameFinal)
	if _, err := dst.Write(final); err != nil {
		return err
	}
	// Authenticate the end marker. Without this tag, a valid prefix of a
	// stream could be truncated and accepted by Decrypt as complete.
	finalTag := aead.Seal(nil, streamNonce(noncePrefix, index), nil, frameAAD(header, index, frameFinal))
	_, err = dst.Write(finalTag)
	return err
}

func Decrypt(dst io.Writer, src io.Reader, key []byte) error {
	_, err := DecryptWithInfo(dst, src, key)
	return err
}

// DecryptWithInfo decrypts and authenticates a stream, returning its format
// version and declared chunk size. Decrypt remains as a compatibility wrapper
// for callers that only need the error result.
func DecryptWithInfo(dst io.Writer, src io.Reader, key []byte) (StreamInfo, error) {
	var info StreamInfo
	if dst == nil || src == nil {
		return info, errors.New("source and destination are required")
	}
	aead, err := makeAEAD(key)
	if err != nil {
		return info, err
	}
	header := make([]byte, headerSize)
	if _, err := io.ReadFull(src, header); err != nil {
		return info, fmt.Errorf("read encryption header: %w", err)
	}
	if string(header[:5]) == string(legacyStreamMagic[:]) {
		return info, errors.New("encrypted stream format v1 is unsupported; repack with v2")
	}
	if string(header[:5]) != string(streamMagic[:]) {
		return info, errors.New("invalid encrypted stream magic")
	}
	chunkSize := int(binary.BigEndian.Uint32(header[5:9]))
	if chunkSize < 4096 || chunkSize > maxChunkSize {
		return info, fmt.Errorf("invalid encrypted chunk size %d", chunkSize)
	}
	info = StreamInfo{Version: 2, ChunkSize: chunkSize}
	noncePrefix := header[9:]
	var index uint64
	seenFinal := false
	frameHeader := make([]byte, 4)
	for {
		if _, err := io.ReadFull(src, frameHeader); err != nil {
			return info, fmt.Errorf("read encrypted frame: %w", err)
		}
		raw := binary.BigEndian.Uint32(frameHeader)
		if raw == frameFinal {
			finalTag := make([]byte, aead.Overhead())
			if _, err := io.ReadFull(src, finalTag); err != nil {
				return info, fmt.Errorf("read encrypted final frame: %w", err)
			}
			if _, err := aead.Open(nil, streamNonce(noncePrefix, index), finalTag, frameAAD(header, index, frameFinal)); err != nil {
				return info, fmt.Errorf("authenticate encrypted final frame: %w", err)
			}
			seenFinal = true
			break
		}
		maxFrameLen := uint32(chunkSize + aead.Overhead())
		if raw&frameFinal != 0 || raw < uint32(aead.Overhead()) || raw > maxFrameLen || raw > uint32(maxCiphertextSize) {
			return info, errors.New("invalid encrypted frame length")
		}
		ciphertext := make([]byte, raw)
		if _, err := io.ReadFull(src, ciphertext); err != nil {
			return info, fmt.Errorf("read encrypted frame payload: %w", err)
		}
		plain, err := aead.Open(nil, streamNonce(noncePrefix, index), ciphertext, frameAAD(header, index, raw))
		if err != nil {
			return info, fmt.Errorf("decrypt frame %d: %w", index, err)
		}
		if len(plain) > chunkSize {
			return info, errors.New("decrypted frame exceeds declared chunk size")
		}
		if _, err := dst.Write(plain); err != nil {
			return info, err
		}
		index++
	}
	if !seenFinal {
		return info, errors.New("encrypted stream has no final frame")
	}
	var extra [1]byte
	if n, err := src.Read(extra[:]); n != 0 || (err != io.EOF && err != nil) {
		return info, errors.New("encrypted stream has trailing data")
	}
	return info, nil
}

func streamNonce(prefix []byte, index uint64) []byte {
	// Derive a distinct 96-bit GCM nonce for every frame from a random
	// 96-bit stream prefix and the frame index. The prefix is public, but its
	// full 96 bits make accidental reuse across streams computationally
	// negligible even when the same AES key is reused for many bundles.
	h := sha256.New()
	_, _ = h.Write(prefix)
	var indexBytes [8]byte
	binary.BigEndian.PutUint64(indexBytes[:], index)
	_, _ = h.Write(indexBytes[:])
	sum := h.Sum(nil)
	return append([]byte(nil), sum[:12]...)
}

func frameAAD(header []byte, index uint64, frameLen uint32) []byte {
	aad := make([]byte, len(header)+8+4)
	copy(aad, header)
	binary.BigEndian.PutUint64(aad[len(header):], index)
	binary.BigEndian.PutUint32(aad[len(header)+8:], frameLen)
	return aad
}
