package sessions

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

const chunkSize = 64 << 10

// Body files consist of authenticated, ordered chunks and a required final
// empty chunk. This permits bounded-memory recording and detects truncation.
// The same byte format is stored as a BLOB inside the gateway database; while
// a request streams, the writer targets a transient temp file so a crash
// never loses the partial body (it is imported on restart).
type bodyWriter struct {
	file    *os.File
	aead    cipher.AEAD
	context string
	index   uint64
	bytes   int64
	err     error
}

func createBody(aead cipher.AEAD, path, context string) (*bodyWriter, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	return &bodyWriter{file: f, aead: aead, context: context}, nil
}

func (w *bodyWriter) chunk(plain []byte) error {
	nonce := make([]byte, w.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := w.aead.Seal(nonce, nonce, plain, []byte(fmt.Sprintf("%s/%d", w.context, w.index)))
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(sealed)))
	if _, err := w.file.Write(size[:]); err != nil {
		return err
	}
	if _, err := w.file.Write(sealed); err != nil {
		return err
	}
	w.index++
	return nil
}

func (w *bodyWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n := 0
	for len(p) > 0 {
		end := len(p)
		if end > chunkSize {
			end = chunkSize
		}
		if err := w.chunk(p[:end]); err != nil {
			w.err = err
			return n, err
		}
		n += end
		w.bytes += int64(end)
		p = p[end:]
	}
	return n, nil
}

func (w *bodyWriter) Close() error {
	if w.err == nil {
		w.err = w.chunk(nil)
	}
	if w.err == nil {
		w.err = w.file.Sync()
	}
	err := w.file.Close()
	if w.err == nil {
		w.err = err
	}
	return w.err
}

// Bytes returns the full chunked-encrypted body bytes (the format stored in
// the database). It must be called after Close.
func (w *bodyWriter) Bytes() ([]byte, error) {
	return os.ReadFile(w.file.Name())
}

// RewriteBody re-encrypts a legacy chunked body from an old key into the
// current key, preserving the plaintext byte-for-byte. Used by migration.
func RewriteBody(old []byte, oldAEAD, newAEAD cipher.AEAD, context string) ([]byte, error) {
	reader := &bodyReader{reader: &bytesReader{b: old}, aead: oldAEAD, context: context}
	plain, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	buf := &byteBuffer{}
	w := &bodyWriterBuffer{buf: buf, aead: newAEAD, context: context}
	if _, err := w.Write(plain); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// bytesReader adapts a []byte to io.Reader (avoids importing bytes at call
// sites that only need ReadBody).
type bytesReader struct{ b []byte }

func (r *bytesReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

type byteBuffer struct{ b []byte }

func (b *byteBuffer) Write(p []byte) (int, error) { b.b = append(b.b, p...); return len(p), nil }
func (b *byteBuffer) Bytes() []byte               { return b.b }

// bodyWriterBuffer is bodyWriter over an in-memory buffer (migration only).
type bodyWriterBuffer struct {
	buf     *byteBuffer
	aead    cipher.AEAD
	context string
	index   uint64
	err     error
}

func (w *bodyWriterBuffer) chunk(plain []byte) error {
	nonce := make([]byte, w.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := w.aead.Seal(nonce, nonce, plain, []byte(fmt.Sprintf("%s/%d", w.context, w.index)))
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(sealed)))
	if _, err := w.buf.Write(size[:]); err != nil {
		return err
	}
	if _, err := w.buf.Write(sealed); err != nil {
		return err
	}
	w.index++
	return nil
}

func (w *bodyWriterBuffer) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n := 0
	for len(p) > 0 {
		end := len(p)
		if end > chunkSize {
			end = chunkSize
		}
		if err := w.chunk(p[:end]); err != nil {
			w.err = err
			return n, err
		}
		n += end
		p = p[end:]
	}
	return n, nil
}

func (w *bodyWriterBuffer) Close() error {
	if w.err == nil {
		w.err = w.chunk(nil)
	}
	return w.err
}

type bodyReader struct {
	reader  io.Reader
	closer  io.Closer
	aead    cipher.AEAD
	context string
	index   uint64
	pending []byte
	done    bool
}

func (r *bodyReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) == 0 && !r.done {
		var size [4]byte
		if _, err := io.ReadFull(r.reader, size[:]); err != nil {
			return 0, io.ErrUnexpectedEOF
		}
		n := int(binary.BigEndian.Uint32(size[:]))
		if n < r.aead.NonceSize()+r.aead.Overhead() || n > chunkSize+r.aead.NonceSize()+r.aead.Overhead() {
			return 0, errors.New("invalid encrypted session body chunk")
		}
		sealed := make([]byte, n)
		if _, err := io.ReadFull(r.reader, sealed); err != nil {
			return 0, io.ErrUnexpectedEOF
		}
		plain, err := r.aead.Open(nil, sealed[:r.aead.NonceSize()], sealed[r.aead.NonceSize():], []byte(fmt.Sprintf("%s/%d", r.context, r.index)))
		if err != nil {
			return 0, errors.New("session body authentication failed")
		}
		r.index++
		if len(plain) == 0 {
			var extra [1]byte
			if n, err := r.reader.Read(extra[:]); n != 0 || err != io.EOF {
				return 0, errors.New("data after session body terminator")
			}
			r.done = true
		} else {
			r.pending = plain
		}
	}
	if len(r.pending) == 0 && r.done {
		return 0, io.EOF
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *bodyReader) Close() error {
	if r.closer != nil {
		return r.closer.Close()
	}
	return nil
}
