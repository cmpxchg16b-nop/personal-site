package sipbot

import (
	"testing"
)

// TestParseMaxMessageBytes covers the size knob's validation: unset
// selects the default, an explicit value must be a byte count a UDP
// datagram can carry.
func TestParseMaxMessageBytes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      int
		want    int
		wantErr bool
	}{
		{"unset selects the default", 0, DefaultMaxMessageBytes, false},
		{"one byte", 1, 1, false},
		{"the largest UDP payload", 65507, 65507, false},
		{"negative", -1, 0, true},
		{"past the largest UDP payload", 65508, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseMaxMessageBytes(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseMaxMessageBytes(%d) = %d, want an error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMaxMessageBytes(%d): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ParseMaxMessageBytes(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestRenderCPIM is the reduced message/cpim render's table (§7.3): the
// inner From/To are kept verbatim, the first text part is the message,
// attachments and signatures are stripped, encrypted and text-less
// bodies are unsupported, a malformed one is an error.
func TestRenderCPIM(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string // the exact rendered chat text
		// unsupported (errCPIMUnsupported) vs malformed (any other error)
		// both read as "an error" here — the 400/415 distinction is the
		// handler's; unsupported is asserted explicitly below.
		wantErr         bool
		wantUnsupported bool
	}{
		{
			name: "a plain text entity",
			body: "From: Alice <sip:alice@example.com>\r\n" +
				"To: Bob <sip:bob@example.com>\r\n" +
				"\r\n" +
				"Content-Type: text/plain; charset=utf-8\r\n" +
				"\r\n" +
				"Hello Bob\r\n",
			want: "Message from Alice <sip:alice@example.com> to Bob <sip:bob@example.com>:\n\nHello Bob",
		},
		{
			name: "the entity's absent content type defaults to text/plain",
			body: "From: Alice <sip:alice@example.com>\r\n\r\nhello\r\n",
			want: "Message from Alice <sip:alice@example.com>:\n\nhello",
		},
		{
			name: "a multipart/mixed's attachment is stripped",
			body: "From: Alice <sip:alice@example.com>\r\n" +
				"To: Bob <sip:bob@example.com>\r\n" +
				"\r\n" +
				"Content-Type: multipart/mixed; boundary=bb\r\n" +
				"\r\n" +
				"--bb\r\n" +
				"Content-Type: text/plain\r\n" +
				"\r\n" +
				"see the photo\r\n" +
				"--bb\r\n" +
				"Content-Type: application/octet-stream\r\n" +
				"Content-Transfer-Encoding: base64\r\n" +
				"\r\n" +
				"aGVscGxl" + "c3Nib2R5\r\n" +
				"--bb--\r\n",
			want: "Message from Alice <sip:alice@example.com> to Bob <sip:bob@example.com>:\n\nsee the photo",
		},
		{
			name: "a multipart/signed's signature is stripped",
			body: "From: Alice <sip:alice@example.com>\r\n" +
				"\r\n" +
				"Content-Type: multipart/signed; boundary=ss; protocol=\"application/pkcs7-signature\"\r\n" +
				"\r\n" +
				"--ss\r\n" +
				"Content-Type: text/plain\r\n" +
				"\r\n" +
				"signed words\r\n" +
				"--ss\r\n" +
				"Content-Type: application/pkcs7-signature\r\n" +
				"\r\n" +
				"c2ln\r\n" +
				"--ss--\r\n",
			want: "Message from Alice <sip:alice@example.com>:\n\nsigned words",
		},
		{
			name: "a nested multipart is followed",
			body: "From: Alice <sip:alice@example.com>\r\n" +
				"\r\n" +
				"Content-Type: multipart/mixed; boundary=oo\r\n" +
				"\r\n" +
				"--oo\r\n" +
				"Content-Type: multipart/alternative; boundary=ii\r\n" +
				"\r\n" +
				"--ii\r\n" +
				"Content-Type: text/plain\r\n" +
				"\r\n" +
				"nested hello\r\n" +
				"--ii--\r\n" +
				"--oo--\r\n",
			want: "Message from Alice <sip:alice@example.com>:\n\nnested hello",
		},
		{
			name: "a base64 text part is decoded",
			body: "From: Alice <sip:alice@example.com>\r\n" +
				"\r\n" +
				"Content-Type: multipart/mixed; boundary=bb\r\n" +
				"\r\n" +
				"--bb\r\n" +
				"Content-Type: text/plain\r\n" +
				"Content-Transfer-Encoding: base64\r\n" +
				"\r\n" +
				"aGkgdGhlcmU=\r\n" +
				"--bb--\r\n",
			want: "Message from Alice <sip:alice@example.com>:\n\nhi there",
		},
		{
			name: "a quoted-printable text part is decoded",
			body: "From: Alice <sip:alice@example.com>\r\n" +
				"\r\n" +
				"Content-Type: text/plain; charset=utf-8\r\n" +
				"Content-Transfer-Encoding: quoted-printable\r\n" +
				"\r\n" +
				"Gr=C3=BC=C3=9F Gott\r\n",
			want: "Message from Alice <sip:alice@example.com>:\n\nGrüß Gott",
		},
		{
			name: "a missing From falls back to the SIP From",
			body: "To: Bob <sip:bob@example.com>\r\n\r\nContent-Type: text/plain\r\n\r\nhey\r\n",
			want: "Message from sip:caller@pbx.example.com to Bob <sip:bob@example.com>:\n\nhey",
		},
		{
			name:            "an encrypted entity is unsupported",
			body:            "From: Alice <sip:alice@example.com>\r\n\r\nContent-Type: application/pkcs7-mime\r\n\r\nzJpZGVu\r\n",
			wantErr:         true,
			wantUnsupported: true,
		},
		{
			name:            "an encrypted multipart is unsupported",
			body:            "From: Alice <sip:alice@example.com>\r\n\r\nContent-Type: multipart/encrypted; boundary=ee\r\n\r\n--ee\r\nContent-Type: text/plain\r\n\r\nhidden\r\n--ee--\r\n",
			wantErr:         true,
			wantUnsupported: true,
		},
		{
			name:            "a multipart without any text part is unsupported",
			body:            "From: Alice <sip:alice@example.com>\r\n\r\nContent-Type: multipart/mixed; boundary=bb\r\n\r\n--bb\r\nContent-Type: image/png\r\n\r\nPNG\r\n--bb--\r\n",
			wantErr:         true,
			wantUnsupported: true,
		},
		{
			name:    "a body without the headers' blank line is malformed",
			body:    "From: Alice <sip:alice@example.com>",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := renderCPIM([]byte(tc.body), "sip:caller@pbx.example.com")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("renderCPIM = %q, want an error", got)
				}
				if tc.wantUnsupported && !isCPIMUnsupported(err) {
					t.Fatalf("renderCPIM's error = %v, want the unsupported one", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("renderCPIM: %v", err)
			}
			if got != tc.want {
				t.Fatalf("renderCPIM = %q, want %q", got, tc.want)
			}
		})
	}
}

// isCPIMUnsupported reports whether err is the parser's
// nothing-forwardable answer (the handler's 415 cue — a malformed body
// is a 400 instead).
func isCPIMUnsupported(err error) bool {
	return err == errCPIMUnsupported
}
