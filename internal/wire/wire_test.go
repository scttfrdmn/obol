package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"testing"
)

// roundTrip writes a frame and reads it back through the same buffer, asserting
// the decoded frame equals what went in for the fields we care about.
func TestRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   *Frame
	}{
		{"gate", GateFrame(&GateRequest{
			Account: "lab_smith", Partition: "sapphire", UID: 1001,
			TimeLimit: 3600, TRES: TRES{CPUs: 8, GPUs: 2, Mem: 32000}, NTasks: 1,
		})},
		{"gate-array", GateFrame(&GateRequest{
			Account: "lab_smith", Partition: "serial-requeue", TimeLimit: 600, NTasks: 100,
		})},
		{"gate-resp-allow", &Frame{MsgKind: KindGate, GateResp: &GateResponse{Allow: true, Token: "budget:abc123"}}},
		{"gate-resp-reject", &Frame{MsgKind: KindGate, GateResp: &GateResponse{Allow: false, Reason: "insufficient budget"}}},
		{"bind", BindFrame(&BindRequest{Token: "budget:abc123", JobID: "4711"})},
		{"bind-resp", &Frame{MsgKind: KindBind, BindResp: &BindResponse{OK: true}}},
		{"settle-complete", SettleFrame(&SettleRequest{JobID: "4711", Kind: SettleComplete, Runtime: 1800})},
		{"settle-infrafail", SettleFrame(&SettleRequest{Token: "budget:xyz", Kind: SettleInfraFail, Elapsed: 900})},
		{"settle-resp", &Frame{MsgKind: KindSettle, SettleResp: &SettleResponse{OK: true}}},
		{"status", StatusFrame("lab_smith")},
		{"status-resp", &Frame{MsgKind: KindStatus, StatusResp: &StatusResponse{
			C: 2, B0: 1000, B: 800, Reserved: 200, TS: 0, TE: 1000, LiveEscrows: 1,
			ConservationOK: true, ConservationSum: 1000, TimeToEmpty: 400,
			Account: "lab_smith", OK: true,
		}}},
		{"topup", TopUpFrame("lab_smith", 5000)},
		{"topup-resp", &Frame{MsgKind: KindTopUp, TopUpResp: &TopUpResponse{OK: true, NewBalance: 15000, NewB0: 15000}}},
		{"list", ListFrame()},
		{"log", LogFrame("lab_smith")},
		{"set-rate", SetRateFrame("lab_smith", 5)},
		{"set-window", SetWindowFrame("lab_smith", 100, 200)},
		{"ack", &Frame{MsgKind: KindSetRate, AckResp: &AckResponse{OK: true}}},
		{"resolve", ResolveFrame(&ResolveRequest{Account: "lab", Partition: "priced", TimeLimit: 100})},
		{"simulate", SimulateFrame(&SimulateRequest{Account: "lab", TimeLimit: 100})},
		{"create", CreateFrame(&CreateRequest{Account: "lab2", Balance: 5000, Rate: 2})},
		{"attach", AttachFrame(&AttachRequest{Account: "lab2", Users: []string{"alice"}})},
		{"attach-resp", &Frame{MsgKind: KindAttach, AttachResp: &AttachResponse{OK: true}}},
		{"ping", PingFrame()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteFrame(&buf, tc.in); err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}
			got, err := ReadFrame(&buf)
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			if got.Version != ProtocolVersion {
				t.Errorf("version = %d, want %d", got.Version, ProtocolVersion)
			}
			if got.MsgKind != tc.in.MsgKind {
				t.Errorf("kind = %q, want %q", got.MsgKind, tc.in.MsgKind)
			}
			// Compare the populated payload by re-marshaling both sides.
			assertFrameEqual(t, tc.in, got)
		})
	}
}

// assertFrameEqual compares two frames field-by-field for the typed payloads.
func assertFrameEqual(t *testing.T, want, got *Frame) {
	t.Helper()
	switch {
	case want.Gate != nil:
		if got.Gate == nil || *got.Gate != *want.Gate {
			t.Errorf("gate: got %+v, want %+v", got.Gate, want.Gate)
		}
	case want.GateResp != nil:
		if got.GateResp == nil || *got.GateResp != *want.GateResp {
			t.Errorf("gate_resp: got %+v, want %+v", got.GateResp, want.GateResp)
		}
	case want.Bind != nil:
		if got.Bind == nil || *got.Bind != *want.Bind {
			t.Errorf("bind: got %+v, want %+v", got.Bind, want.Bind)
		}
	case want.Settle != nil:
		if got.Settle == nil || *got.Settle != *want.Settle {
			t.Errorf("settle: got %+v, want %+v", got.Settle, want.Settle)
		}
	case want.StatusResp != nil:
		if got.StatusResp == nil || *got.StatusResp != *want.StatusResp {
			t.Errorf("status_resp: got %+v, want %+v", got.StatusResp, want.StatusResp)
		}
	case want.TopUp != nil:
		if got.TopUp == nil || *got.TopUp != *want.TopUp {
			t.Errorf("topup: got %+v, want %+v", got.TopUp, want.TopUp)
		}
	case want.TopUpResp != nil:
		if got.TopUpResp == nil || *got.TopUpResp != *want.TopUpResp {
			t.Errorf("topup_resp: got %+v, want %+v", got.TopUpResp, want.TopUpResp)
		}
	case want.SetRate != nil:
		if got.SetRate == nil || *got.SetRate != *want.SetRate {
			t.Errorf("set_rate: got %+v, want %+v", got.SetRate, want.SetRate)
		}
	case want.SetWindow != nil:
		if got.SetWindow == nil || *got.SetWindow != *want.SetWindow {
			t.Errorf("set_window: got %+v, want %+v", got.SetWindow, want.SetWindow)
		}
	case want.AckResp != nil:
		if got.AckResp == nil || *got.AckResp != *want.AckResp {
			t.Errorf("ack_resp: got %+v, want %+v", got.AckResp, want.AckResp)
		}
	}
}

// TestMultipleFramesOneStream confirms records are self-delimiting: several
// frames written back-to-back read back in order.
func TestMultipleFramesOneStream(t *testing.T) {
	var buf bytes.Buffer
	frames := []*Frame{
		GateFrame(&GateRequest{Account: "a", Partition: "p", TimeLimit: 60, NTasks: 1}),
		BindFrame(&BindRequest{Token: "t", JobID: "1"}),
		SettleFrame(&SettleRequest{JobID: "1", Kind: SettleComplete, Runtime: 30}),
	}
	for _, f := range frames {
		if err := WriteFrame(&buf, f); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range frames {
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if got.MsgKind != want.MsgKind {
			t.Errorf("frame %d kind = %q, want %q", i, got.MsgKind, want.MsgKind)
		}
	}
	if _, err := ReadFrame(&buf); !errors.Is(err, io.EOF) {
		t.Errorf("after last frame: err = %v, want io.EOF", err)
	}
}

// TestVersionMismatch confirms a frame stamped with a foreign version is
// rejected with ErrVersion, not silently misparsed. WriteFrame always stamps
// the current version, so we frame the bytes by hand with a foreign one.
func TestVersionMismatch(t *testing.T) {
	payload := mustJSON(t, &Frame{Version: ProtocolVersion + 1, MsgKind: KindPing})
	var buf bytes.Buffer
	writeRaw(&buf, payload)

	_, err := ReadFrame(&buf)
	if !errors.Is(err, ErrVersion) {
		t.Fatalf("err = %v, want ErrVersion", err)
	}
}

// TestCorruptPayload confirms a flipped payload byte fails the crc check.
func TestCorruptPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, PingFrame()); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	b[len(b)-1] ^= 0xFF // corrupt the last payload byte
	_, err := ReadFrame(bytes.NewReader(b))
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}

// TestTruncatedFrame confirms a payload shorter than its length prefix is a
// short read, not a hang or a misparse.
func TestTruncatedFrame(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, PingFrame()); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	truncated := b[:len(b)-2] // drop 2 payload bytes
	_, err := ReadFrame(bytes.NewReader(truncated))
	if !errors.Is(err, ErrShortRead) {
		t.Fatalf("err = %v, want ErrShortRead", err)
	}
}

// TestOversizeLengthPrefix confirms a corrupt/huge length prefix is rejected by
// the size cap rather than triggering a giant allocation.
func TestOversizeLengthPrefix(t *testing.T) {
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], MaxFrameSize+1)
	_, err := ReadFrame(bytes.NewReader(hdr[:]))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

// --- test helpers ---

// mustJSON marshals a frame verbatim (without WriteFrame's version stamping) so
// tests can craft frames with an arbitrary Version field.
func mustJSON(t *testing.T, f *Frame) []byte {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// writeRaw frames an already-marshaled payload with the standard
// [u32 len][u32 crc32] header, bypassing WriteFrame's version stamping.
func writeRaw(buf *bytes.Buffer, payload []byte) {
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(len(payload)))
	binary.LittleEndian.PutUint32(hdr[4:8], crc32.Checksum(payload, crcTable))
	buf.Write(hdr[:])
	buf.Write(payload)
}
