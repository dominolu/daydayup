package observe

import (
	"io"
	"net/http"
)

// CopySSE streams an SSE response to w while observing it. A protocol terminal
// event ends the response even when its HTTP connection remains open. Only
// stop after all bytes in the read have been written and flushed successfully:
// seeing a terminal in an upstream read alone does not prove downstream
// delivery (short writes and flush failures remain failures).
func CopySSE(w http.ResponseWriter, src io.Reader, o *Observer) error {
	buf := make([]byte, 32<<10)
	controller := http.NewResponseController(w)
	for {
		n, readErr := src.Read(buf)
		if n > 0 {
			o.Write(buf[:n])
			written, writeErr := w.Write(buf[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
			if err := controller.Flush(); err != nil {
				return err
			}
			if o.terminal {
				return nil
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}
