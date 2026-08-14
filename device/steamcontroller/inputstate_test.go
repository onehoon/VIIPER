package steamcontroller

import (
	"encoding/binary"
	"testing"
)

func TestInputStateIndependentDigitalTriggers(t *testing.T) {
	const mid = MaxAnalogTriggerRaw / 2
	cases := []struct {
		name           string
		left, right    uint16
		l2, r2         bool
		wantL2, wantR2 bool
	}{
		{name: "mid no digital", left: mid, right: mid},
		{name: "mid left digital", left: mid, right: mid, l2: true, wantL2: true},
		{name: "mid right digital", left: mid, right: mid, r2: true, wantR2: true},
		{name: "max analog", left: MaxAnalogTriggerRaw, right: MaxAnalogTriggerRaw, wantL2: true, wantR2: true},
		{name: "max plus explicit", left: MaxAnalogTriggerRaw, right: MaxAnalogTriggerRaw, l2: true, r2: true, wantL2: true, wantR2: true},
		{name: "zero explicit", l2: true, r2: true, wantL2: true, wantR2: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := InputState{LTrigger: tc.left, RTrigger: tc.right, L2: tc.l2, R2: tc.r2}
			data, err := state.MarshalBinary()
			if err != nil {
				t.Fatal(err)
			}
			if got := data[8]&buttonByte8L2 != 0; got != tc.wantL2 {
				t.Fatalf("L2 report = %t, want %t", got, tc.wantL2)
			}
			if got := data[8]&buttonByte8R2 != 0; got != tc.wantR2 {
				t.Fatalf("R2 report = %t, want %t", got, tc.wantR2)
			}
			if got := binary.LittleEndian.Uint16(data[24:26]); got != tc.left {
				t.Fatalf("left analog primary = %d, want %d", got, tc.left)
			}
			if got := binary.LittleEndian.Uint16(data[50:52]); got != tc.left {
				t.Fatalf("left analog secondary = %d, want %d", got, tc.left)
			}
			if got := binary.LittleEndian.Uint16(data[26:28]); got != tc.right {
				t.Fatalf("right analog primary = %d, want %d", got, tc.right)
			}
			if got := binary.LittleEndian.Uint16(data[52:54]); got != tc.right {
				t.Fatalf("right analog secondary = %d, want %d", got, tc.right)
			}

			var roundTrip InputState
			if err := roundTrip.UnmarshalBinary(data); err != nil {
				t.Fatal(err)
			}
			if roundTrip.L2 != tc.wantL2 || roundTrip.R2 != tc.wantR2 {
				t.Fatalf("round-trip digital triggers = (%t, %t), want (%t, %t)", roundTrip.L2, roundTrip.R2, tc.wantL2, tc.wantR2)
			}
			if roundTrip.LTrigger != tc.left || roundTrip.RTrigger != tc.right {
				t.Fatalf("round-trip analog triggers = (%d, %d), want (%d, %d)", roundTrip.LTrigger, roundTrip.RTrigger, tc.left, tc.right)
			}
		})
	}
}

func TestInputStateUnmarshalMarshalPreservesTriggerBitsAndAnalogCopies(t *testing.T) {
	data := make([]byte, InputReportLen)
	data[8] = buttonByte8L1 | buttonByte8R1 | buttonByte8L2 | buttonByte8R2
	binary.LittleEndian.PutUint16(data[24:26], 1111)
	binary.LittleEndian.PutUint16(data[26:28], 2222)
	binary.LittleEndian.PutUint16(data[50:52], 3333)
	binary.LittleEndian.PutUint16(data[52:54], 4444)
	var state InputState
	if err := state.UnmarshalBinary(data); err != nil {
		t.Fatal(err)
	}
	if !state.L2 || !state.R2 {
		t.Fatalf("unmarshaled digital triggers = (%t, %t)", state.L2, state.R2)
	}
	if state.LTrigger != 3333 || state.RTrigger != 4444 {
		t.Fatalf("unmarshaled analog triggers = (%d, %d)", state.LTrigger, state.RTrigger)
	}
	marshaled, err := state.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if marshaled[8]&(buttonByte8L2|buttonByte8R2) != buttonByte8L2|buttonByte8R2 {
		t.Fatalf("marshaled trigger bits = %#x", marshaled[8])
	}
	for offset, want := range map[int]uint16{24: 3333, 26: 4444, 50: 3333, 52: 4444} {
		if got := binary.LittleEndian.Uint16(marshaled[offset : offset+2]); got != want {
			t.Fatalf("analog at %d = %d, want %d", offset, got, want)
		}
	}
}
