package usbip

import (
	"bytes"
	"errors"
	"testing"
)

type retSubmitTestErrorWriter struct{ err error }

func (w retSubmitTestErrorWriter) Write([]byte) (int, error) { return 0, w.err }

type retSubmitTestCountingWriter struct {
	buf    bytes.Buffer
	writes int
}

func (w *retSubmitTestCountingWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.buf.Write(p)
}

func TestRetSubmitEncodeMatchesIndependentGoldenFrame(t *testing.T) {
	ret := RetSubmit{
		Basic: HeaderBasic{
			Command: 0x01020304,
			Seqnum:  0x11223344,
			Devid:   0x55667788,
			Dir:     0x99aabbcc,
			Ep:      0xddeeff00,
		},
		Status:          -104,
		ActualLength:    0x01234567,
		StartFrame:      0x89abcdef,
		NumberOfPackets: 0x10203040,
		ErrorCount:      0x50607080,
		Padding:         [8]byte{0x90, 0xa1, 0xb2, 0xc3, 0xd4, 0xe5, 0xf6, 0x07},
	}
	want := []byte{
		0x01, 0x02, 0x03, 0x04, 0x11, 0x22, 0x33, 0x44,
		0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc,
		0xdd, 0xee, 0xff, 0x00, 0xff, 0xff, 0xff, 0x98,
		0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
		0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70, 0x80,
		0x90, 0xa1, 0xb2, 0xc3, 0xd4, 0xe5, 0xf6, 0x07,
	}

	dst := bytes.Repeat([]byte{0xee}, 52)
	ret.Encode(dst)
	if !bytes.Equal(dst[:48], want) {
		t.Fatalf("Encode frame = % x, want % x", dst[:48], want)
	}
	if !bytes.Equal(dst[48:], []byte{0xee, 0xee, 0xee, 0xee}) {
		t.Fatalf("Encode modified bytes after the 48-byte header: % x", dst[48:])
	}

	var written bytes.Buffer
	if err := ret.Write(&written); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !bytes.Equal(written.Bytes(), want) {
		t.Fatalf("Write frame = % x, want % x", written.Bytes(), want)
	}
	counting := &retSubmitTestCountingWriter{}
	if err := ret.Write(counting); err != nil {
		t.Fatalf("Write to counting writer: %v", err)
	}
	if counting.writes != 1 {
		t.Fatalf("RetSubmit.Write calls = %d, want one header write", counting.writes)
	}
	if !bytes.Equal(counting.buf.Bytes(), want) {
		t.Fatalf("counted Write frame = % x, want % x", counting.buf.Bytes(), want)
	}

	writeErr := errors.New("simulated writer failure")
	if err := ret.Write(retSubmitTestErrorWriter{err: writeErr}); !errors.Is(err, writeErr) {
		t.Fatalf("Write error = %v, want %v", err, writeErr)
	}
}

func TestRetSubmitEncodeCanonicalEmptySuccessFrame(t *testing.T) {
	ret := RetSubmit{Basic: HeaderBasic{Command: RetSubmitCode}}
	var got [48]byte
	ret.Encode(got[:])

	var want [48]byte
	want[3] = 0x03 // Big-endian RetSubmitCode; all remaining fields are zero.
	if got != want {
		t.Fatalf("canonical empty RET_SUBMIT = % x, want % x", got, want)
	}

	var written bytes.Buffer
	if err := ret.Write(&written); err != nil {
		t.Fatalf("Write canonical empty RET_SUBMIT: %v", err)
	}
	if !bytes.Equal(written.Bytes(), want[:]) {
		t.Fatalf("canonical Write frame = % x, want % x", written.Bytes(), want)
	}
}
