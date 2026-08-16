package certs

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pkgutils "personal-site/pkg/utils"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"
)

// newTestValidator returns a validation context trusting the certificate of a
// fresh random keystore, along with the keystore itself (so tests can sign
// documents) and its certificate (so tests can check the reported cert).
func newTestValidator(t *testing.T) (validator *dsig.ValidationContext, keyStore dsig.X509KeyStore, cert *x509.Certificate) {
	t.Helper()
	keyStore = dsig.RandomKeyStoreForTest()
	_, certDER, err := keyStore.GetKeyPair()
	if err != nil {
		t.Fatalf("GetKeyPair: %v", err)
	}
	cert, err = x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	store := &dsig.MemoryX509CertificateStore{Roots: []*x509.Certificate{cert}}
	return dsig.NewDefaultValidationContext(store), keyStore, cert
}

// signedDocument builds a small signed exam report with the given title and
// returns its serialization.
func signedDocument(t *testing.T, keyStore dsig.X509KeyStore, title string) []byte {
	t.Helper()
	doc := etree.NewDocument()
	root := doc.CreateElement("examreport")
	root.CreateAttr("id", "r1")
	person := root.CreateElement("examtaker").CreateElement("person")
	person.CreateAttr("name", "alice")
	person.CreateAttr("fistname", "Alice")
	person.CreateAttr("lastname", "Smith")
	person.CreateAttr("email", "alice@example.com")
	root.CreateElement("examid").SetText("exam-r1")
	root.CreateElement("title").SetText(title)
	root.CreateElement("examcode").SetText("300-620")
	root.CreateElement("examsessionid").SetText("sess-r1")
	root.CreateElement("finishedat").SetText("1700000000000")
	assessment := root.CreateElement("assessment")
	assessment.CreateElement("overallresult").SetText("pass")
	score := assessment.CreateElement("scoreresult")
	score.CreateAttr("earnedScore", "7")
	score.CreateAttr("totalScore", "10")
	signed, err := dsig.NewDefaultSigningContext(keyStore).SignEnveloped(root)
	if err != nil {
		t.Fatalf("SignEnveloped: %v", err)
	}
	signedDoc := etree.NewDocument()
	signedDoc.SetRoot(signed)
	raw, err := signedDoc.WriteToBytes()
	if err != nil {
		t.Fatalf("WriteToBytes: %v", err)
	}
	return raw
}

// multipartBody packs content as a multipart/form-data body with a single
// file part under the given form field, returning the content type and body.
func multipartBody(t *testing.T, field, filename string, content []byte) (contentType string, body *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return mw.FormDataContentType(), &buf
}

// serveRequest runs one request against h and returns the recorder. body is
// an io.Reader (rather than a concrete buffer) so a nil body reaches
// httptest.NewRequest as a true nil interface.
func serveRequest(h http.Handler, method, contentType string, body io.Reader) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/certs/verify", body)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder) verifyResponse {
	t.Helper()
	var resp verifyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, rec.Body.String())
	}
	return resp
}

// TestVerify_ValidSignature posts a properly signed document and expects a
// positive verification naming the signing certificate.
func TestVerify_ValidSignature(t *testing.T) {
	validator, keyStore, cert := newTestValidator(t)
	h := NewCertVerificationHandler(validator)

	signed := signedDocument(t, keyStore, "Implementing Cisco ACI")
	contentType, body := multipartBody(t, fileFormField, "report.xml", signed)
	rec := serveRequest(h, http.MethodPost, contentType, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	if !resp.Valid {
		t.Fatalf("valid = false, want true (error %q)", resp.Error)
	}
	if resp.Certificate == nil {
		t.Fatal("certificate is nil, want the verified cert")
	}
	got := resp.Certificate
	if got.Subject != cert.Subject.String() {
		t.Errorf("subject = %q, want %q", got.Subject, cert.Subject.String())
	}
	if got.Issuer != cert.Issuer.String() {
		t.Errorf("issuer = %q, want %q", got.Issuer, cert.Issuer.String())
	}
	if got.SerialNumber != cert.SerialNumber.String() {
		t.Errorf("serial = %q, want %q", got.SerialNumber, cert.SerialNumber.String())
	}
	sum := sha256.Sum256(cert.Raw)
	wantFp := strings.ToUpper(hex.EncodeToString(sum[:]))
	if strings.ReplaceAll(got.SHA256Fingerprint, ":", "") != wantFp {
		t.Errorf("fingerprint = %q, want the sha256 of the cert (%q)", got.SHA256Fingerprint, wantFp)
	}
	if _, err := time.Parse(time.RFC3339, got.NotBefore); err != nil {
		t.Errorf("not_before = %q, want RFC3339: %v", got.NotBefore, err)
	}
	if _, err := time.Parse(time.RFC3339, got.NotAfter); err != nil {
		t.Errorf("not_after = %q, want RFC3339: %v", got.NotAfter, err)
	}

	// The response carries the verified exam report itself.
	if resp.Report == nil {
		t.Fatal("report is nil, want the verified exam report")
	}
	rep := resp.Report
	if rep.Id != "r1" {
		t.Errorf("report id = %q, want r1", rep.Id)
	}
	if rep.Title != "Implementing Cisco ACI" {
		t.Errorf("report title = %q, want %q", rep.Title, "Implementing Cisco ACI")
	}
	if rep.ExamCode != "300-620" {
		t.Errorf("report exam code = %q, want 300-620", rep.ExamCode)
	}
	if len(rep.ExamTaker.Persons) != 1 || rep.ExamTaker.Persons[0].Name != "alice" {
		t.Errorf("report exam taker = %+v, want person alice", rep.ExamTaker.Persons)
	}
	if rep.FinishedAt != 1700000000000 {
		t.Errorf("report finishedAt = %d, want 1700000000000", rep.FinishedAt)
	}
	if rep.Assessment.OverallResult == nil || string(*rep.Assessment.OverallResult) != "pass" {
		t.Errorf("report overall result = %v, want pass", rep.Assessment.OverallResult)
	}
	if rep.Assessment.ScoreResult == nil ||
		rep.Assessment.ScoreResult.EarnedScore != 7 || rep.Assessment.ScoreResult.TotalScore != 10 {
		t.Errorf("report score = %+v, want 7/10", rep.Assessment.ScoreResult)
	}
}

// TestVerify_UnsignedDocument posts a well-formed but unsigned document and
// expects a successful API call reporting a failed verification.
func TestVerify_UnsignedDocument(t *testing.T) {
	validator, _, _ := newTestValidator(t)
	h := NewCertVerificationHandler(validator)

	contentType, body := multipartBody(t, fileFormField, "report.xml", []byte(`<examreport id="r1"><title>T</title></examreport>`))
	rec := serveRequest(h, http.MethodPost, contentType, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	if resp.Valid {
		t.Fatal("valid = true, want false for an unsigned document")
	}
	if resp.Error == "" {
		t.Error("error is empty, want the verification failure reason")
	}
	if resp.Certificate != nil {
		t.Errorf("certificate = %+v, want nil for a failed verification", resp.Certificate)
	}
	if resp.Report != nil {
		t.Errorf("report = %+v, want nil for a failed verification", resp.Report)
	}
}

// TestVerify_TamperedDocument posts a signed document whose content was
// altered after signing; the digest no longer matches.
func TestVerify_TamperedDocument(t *testing.T) {
	validator, keyStore, _ := newTestValidator(t)
	h := NewCertVerificationHandler(validator)

	signed := signedDocument(t, keyStore, "Implementing Cisco ACI")
	tampered := bytes.Replace(signed, []byte("Implementing"), []byte("TamperedWith"), 1)
	if bytes.Equal(tampered, signed) {
		t.Fatal("tampering did not change the document")
	}
	contentType, body := multipartBody(t, fileFormField, "report.xml", tampered)
	rec := serveRequest(h, http.MethodPost, contentType, body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	resp := decodeResponse(t, rec)
	if resp.Valid {
		t.Fatal("valid = true, want false for a tampered document")
	}
	if resp.Error == "" {
		t.Error("error is empty, want the verification failure reason")
	}
}

// TestVerify_MissingFile posts a multipart body without the "file" field.
func TestVerify_MissingFile(t *testing.T) {
	validator, _, _ := newTestValidator(t)
	h := NewCertVerificationHandler(validator)

	contentType, body := multipartBody(t, "document", "report.xml", []byte(`<examreport id="r1"/>`))
	rec := serveRequest(h, http.MethodPost, contentType, body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
	var errResp pkgutils.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if errResp.Error == "" {
		t.Error("err is empty, want the reason")
	}
}

// TestVerify_NotXML posts a file that is not well-formed XML.
func TestVerify_NotXML(t *testing.T) {
	validator, _, _ := newTestValidator(t)
	h := NewCertVerificationHandler(validator)

	contentType, body := multipartBody(t, fileFormField, "report.xml", []byte("this is not xml"))
	rec := serveRequest(h, http.MethodPost, contentType, body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

// TestVerify_MethodNotAllowed confirms only POST is accepted.
func TestVerify_MethodNotAllowed(t *testing.T) {
	validator, _, _ := newTestValidator(t)
	h := NewCertVerificationHandler(validator)

	rec := serveRequest(h, http.MethodGet, "", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body: %s", rec.Code, rec.Body.String())
	}
	if allow := rec.Header().Get("Allow"); allow != "POST" {
		t.Errorf("Allow = %q, want POST", allow)
	}
}
