package ct_test

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/JoshuaMart/recon/internal/ct"
)

// rawFixture is the pinned stream as the socket delivers it, one message per line.
func rawFixture(t *testing.T) [][]byte {
	t.Helper()

	data, err := os.ReadFile("testdata/stream.jsonl")
	if err != nil {
		t.Fatalf("read the pinned stream: %v", err)
	}
	var lines [][]byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) > 0 {
			lines = append(lines, line)
		}
	}
	return lines
}

// The decoder replaced encoding/json for speed only. Over the real stream it must
// produce the same Frame, field for field, or a faster matcher reads different
// certificates.
func TestTheDecoderAgreesWithEncodingJSONOnTheStream(t *testing.T) {
	var decoder ct.Decoder
	for i, line := range rawFixture(t) {
		var want, got ct.Frame
		if err := json.Unmarshal(line, &want); err != nil {
			t.Fatalf("frame %d: encoding/json: %v", i, err)
		}
		if err := decoder.Decode(line, &got); err != nil {
			t.Fatalf("frame %d: decoder: %v", i, err)
		}
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("frame %d differs:\nencoding/json %+v\ndecoder       %+v", i, want, got)
		}
	}
}

// The frame is reused across messages, so nothing from the previous one may survive.
func TestTheDecoderLeavesNothingFromThePreviousFrame(t *testing.T) {
	var (
		decoder ct.Decoder
		frame   ct.Frame
	)
	full := `{"message_type":"certificate_update","data":{"update_type":"X509LogEntry","cert_index":7,` +
		`"leaf_cert":{"all_domains":["a.acme.test"],"issuer":{"CN":"R3","O":"Let's Encrypt"}},` +
		`"source":{"name":"Argon","url":"https://ct.example"}}}`
	if err := decoder.Decode([]byte(full), &frame); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode([]byte(`{"message_type":"heartbeat"}`), &frame); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(frame, ct.Frame{MessageType: "heartbeat"}) {
		t.Fatalf("a field of the previous frame survived: %+v", frame)
	}
}

// What encoding/json refused, the decoder refuses: a malformed message is counted as
// undecodable, never read as a certificate with no names.
func TestTheDecoderRefusesWhatEncodingJSONRefused(t *testing.T) {
	cases := map[string]string{
		"not json":             `certificate_update`,
		"truncated":            `{"message_type":"certificate_update","data":{`,
		"names not a list":     `{"message_type":"certificate_update","data":{"leaf_cert":{"all_domains":"a.acme.test"}}}`,
		"a name not a string":  `{"message_type":"certificate_update","data":{"leaf_cert":{"all_domains":[42]}}}`,
		"index not a number":   `{"message_type":"certificate_update","data":{"cert_index":"7"}}`,
		"message type numeric": `{"message_type":1}`,
	}
	var decoder ct.Decoder
	for name, input := range cases {
		var frame ct.Frame
		if err := json.Unmarshal([]byte(input), &frame); err == nil {
			t.Fatalf("%s: encoding/json accepted it, so the case proves nothing", name)
		}
		if err := decoder.Decode([]byte(input), &frame); err == nil {
			t.Errorf("%s: the decoder accepted what encoding/json refused", name)
		}
	}
}

// What the production profile measured, kept runnable: decoding one real message.
func BenchmarkDecode(b *testing.B) {
	data, err := os.ReadFile("testdata/stream.jsonl")
	if err != nil {
		b.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))

	var (
		decoder ct.Decoder
		frame   ct.Frame
	)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if err := decoder.Decode(lines[i%len(lines)], &frame); err != nil {
			b.Fatal(err)
		}
	}
}
