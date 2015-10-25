// Copyright 2015 Ulrich Kunitz. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package lzma

import (
	"bytes"
	"io/ioutil"
	"testing"
)

var testString = `LZMA decoder test example
=========================
! LZMA ! Decoder ! TEST !
=========================
! TEST ! LZMA ! Decoder !
=========================
---- Test Line 1 --------
=========================
---- Test Line 2 --------
=========================
=== End of test file ====
=========================
`

func TestEncoderCycle(t *testing.T) {
	const dictCap = minDictCap
	encoderDict, err := NewEncoderDict(dictCap, dictCap+1024)
	if err != nil {
		t.Fatal(err)
	}
	props, err := NewProperties(2, 0, 2)
	if err != nil {
		t.Fatalf("NewProperties error %s", err)
	}
	state := NewState(props)
	var buf bytes.Buffer
	w := new(Encoder)
	params := CodecParams{EOSMarker: true, Size: -1}
	if err = w.Init(&buf, state, encoderDict, params); err != nil {
		t.Fatalf("w.Init error %s", err)
	}
	orig := []byte(testString)
	t.Logf("len(orig) %d", len(orig))
	n, err := w.Write(orig)
	if err != nil {
		t.Fatalf("w.Write error %s", err)
	}
	if n != len(orig) {
		t.Fatalf("w.Write returned %d; want %d", n, len(orig))
	}
	if err = w.Close(); err != nil {
		t.Fatalf("w.Close error %s", err)
	}
	t.Logf("buf.Len() %d len(orig) %d", buf.Len(), len(orig))
	if buf.Len() > len(orig) {
		t.Errorf("buf.Len()=%d bigger then len(orig)=%d", buf.Len(),
			len(orig))
	}
	decoderDict, err := NewDecoderDict(dictCap, dictCap)
	if err != nil {
		t.Fatalf("NewDecoderDict error %s", err)
	}
	// state.Reset()
	state = NewState(props)
	r := new(Decoder)
	if err = r.Init(&buf, state, decoderDict, params); err != nil {
		t.Fatalf("Init error %s", err)
	}
	decoded, err := ioutil.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll(lr) error %s", err)
	}
	t.Logf("%s", decoded)
	if len(orig) != len(decoded) {
		t.Fatalf("length decoded is %d; want %d", len(decoded),
			len(orig))
	}
	if !bytes.Equal(orig, decoded) {
		t.Fatalf("decoded file differs from original")
	}
}
