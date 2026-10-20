package observe

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
)

type dataAndErrorReader struct {
	data []byte
	err  error
}

func (r *dataAndErrorReader) Read(b []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, errors.New("unexpected read after terminal")
	}
	n := copy(b, r.data)
	r.data = r.data[n:]
	return n, r.err
}

func TestCopySSETerminalWithTrailingReadErrorStillDelivered(t *testing.T) {
	o := NewObserver(true, false)
	r := &dataAndErrorReader{data: []byte("data: [DONE]\n\n"), err: context.Canceled}
	w := httptest.NewRecorder()
	if err := CopySSE(w, r, o); err != nil || !o.terminal || w.Body.String() != "data: [DONE]\n\n" {
		t.Fatal("terminal bytes returned with a read error were discarded", err)
	}
}
