// Package certs exposes the /api/certs/verify HTTP endpoint, letting the
// holder of a signed exam report verify its enveloped XMLDSIG signature
// against the server's trusted certificates.
//
// The signed document travels as an ordinary multipart file upload. The
// endpoint requires no session: the signature itself is the proof being
// checked, so there is nothing to authorize.
//
// Wire shape:
//
//	POST /api/certs/verify   multipart/form-data "file" (signed XML document)
//	                         -> 200 {"valid":true,"certificate":{...},
//	                            "report":{...}} — report is the verified exam
//	                            report, present when the signed document is one
//	                         -> 200 {"valid":false,"error":"..."} on a failed
//	                            or missing signature
//	                         -> 400 {"err":...} on a malformed request
//	other methods            -> 405
package certs

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"strings"
	"time"

	"personal-site/pkg/models/examreport"
	pkgmodelssigner "personal-site/pkg/models/signer"
	pkgutils "personal-site/pkg/utils"

	"github.com/beevik/etree"
	dsig "github.com/russellhaering/goxmldsig"
	"github.com/russellhaering/goxmldsig/etreeutils"
)

// maxUploadSize is the most bytes a verification request body may hold. A
// signed exam report is a small XML document; this cap is generous.
const maxUploadSize int64 = 4 << 20 // 4 MiB

// maxUploadMemory is the most bytes of a multipart upload held in RAM before
// the rest spills to a temp file.
const maxUploadMemory int64 = 1 << 20 // 1 MiB

// fileFormField is the multipart form field name carrying the uploaded file.
const fileFormField = "file"

// CertVerificationHandler is an http.Handler that verifies the enveloped
// XMLDSIG signature of an uploaded XML document. It is mounted at
// /api/certs/verify and answers POST only.
type CertVerificationHandler struct {
	validator pkgmodelssigner.XMLETreeSignatureValidator
}

// NewCertVerificationHandler constructs a CertVerificationHandler. validator
// verifies the signature of uploaded documents (typically a
// *dsig.ValidationContext built from the server's trusted certificates).
func NewCertVerificationHandler(validator pkgmodelssigner.XMLETreeSignatureValidator) *CertVerificationHandler {
	return &CertVerificationHandler{validator: validator}
}

// certificateDTO is the JSON shape of the signing certificate reported on a
// successful verification.
type certificateDTO struct {
	Subject           string `json:"subject"`
	Issuer            string `json:"issuer"`
	SerialNumber      string `json:"serial_number"`
	NotBefore         string `json:"not_before"`
	NotAfter          string `json:"not_after"`
	SHA256Fingerprint string `json:"sha256_fingerprint"`
}

// verifyResponse is the JSON body of POST /api/certs/verify. A failed
// verification is still a successful API call: Valid reports the outcome and
// Error carries the reason. Certificate is present only when the document
// verified and its signature carried an X.509 certificate in its KeyInfo.
// Report is the verified exam report itself — the signature-stripped document
// the signature vouches for — present when the document verified and parses
// as an <examreport>.
type verifyResponse struct {
	Valid       bool                   `json:"valid"`
	Error       string                 `json:"error,omitempty"`
	Certificate *certificateDTO        `json:"certificate,omitempty"`
	Report      *examreport.ExamReport `json:"report,omitempty"`
}

// ServeHTTP implements http.Handler. Only POST is accepted; everything else
// gets 405 with an Allow header.
func (h *CertVerificationHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	h.handleVerify(w, r)
}

// handleVerify reads the uploaded file from the "file" form field, parses it
// as XML, and verifies its enveloped signature. Malformed requests get 400; a
// well-formed document whose signature is missing or does not verify gets 200
// with {"valid":false,"error":...}.
func (h *CertVerificationHandler) handleVerify(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadMemory); err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart request: "+err.Error())
		return
	}

	file, _, err := r.FormFile(fileFormField)
	if err != nil {
		writeError(w, http.StatusBadRequest, `missing "file" field: `+err.Error())
		return
	}
	defer file.Close()

	doc := etree.NewDocument()
	if _, err := doc.ReadFrom(file); err != nil {
		writeError(w, http.StatusBadRequest, "uploaded file is not well-formed XML: "+err.Error())
		return
	}
	root := doc.Root()
	if root == nil {
		writeError(w, http.StatusBadRequest, "uploaded file holds no XML document")
		return
	}

	// Best effort: lift the signing certificate out of the signature's
	// KeyInfo so a verified response can name it. Its absence never fails the
	// request.
	cert := signingCertificate(root)

	validated, err := h.validator.Validate(root)
	if err != nil {
		writeJSON(w, verifyResponse{Valid: false, Error: err.Error()})
		return
	}
	resp := verifyResponse{Valid: true}
	if cert != nil {
		resp.Certificate = certificateToDTO(cert)
	}
	// The verified content is the payload the caller actually cares about —
	// for this server, an exam report. A signed document that is not an
	// <examreport> still verifies; it just gets no report in the response.
	if report, err := examReportFromElement(validated); err == nil {
		resp.Report = report
	}
	writeJSON(w, resp)
}

// signingCertificate extracts the X.509 certificate carried in the
// signature's KeyInfo, or nil when the document has none or it cannot be
// parsed.
func signingCertificate(root *etree.Element) *x509.Certificate {
	el, err := etreeutils.NSFindOne(root, dsig.Namespace, "X509Certificate")
	if err != nil || el == nil {
		return nil
	}
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(el.Text()))
	if err != nil {
		return nil
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil
	}
	return cert
}

// examReportFromElement converts the validated (signature-stripped) element
// returned by the validator back into the exam report model: the element is
// re-serialized and unmarshaled, exactly reversing the serialize-reparse-sign
// pipeline that produced the signed document. It fails when the document is
// not an <examreport>.
func examReportFromElement(el *etree.Element) (*examreport.ExamReport, error) {
	doc := etree.NewDocument()
	doc.SetRoot(el.Copy())
	raw, err := doc.WriteToBytes()
	if err != nil {
		return nil, err
	}
	var report examreport.ExamReport
	if err := xml.Unmarshal(raw, &report); err != nil {
		return nil, err
	}
	return &report, nil
}

// certificateToDTO renders cert as the wire shape reported on verification.
func certificateToDTO(cert *x509.Certificate) *certificateDTO {
	fingerprint := sha256.Sum256(cert.Raw)
	return &certificateDTO{
		Subject:           cert.Subject.String(),
		Issuer:            cert.Issuer.String(),
		SerialNumber:      cert.SerialNumber.String(),
		NotBefore:         cert.NotBefore.UTC().Format(time.RFC3339),
		NotAfter:          cert.NotAfter.UTC().Format(time.RFC3339),
		SHA256Fingerprint: colonHex(fingerprint[:]),
	}
}

// colonHex renders b as uppercase hex byte pairs joined by colons, the
// customary display form of a certificate fingerprint.
func colonHex(b []byte) string {
	pairs := strings.ToUpper(hex.EncodeToString(b))
	out := make([]string, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, pairs[i:i+2])
	}
	return strings.Join(out, ":")
}

// writeJSON encodes v as the response body.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes an error response in the project's standard {"err":...}
// shape (see pkgutils.ErrorResponse).
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(pkgutils.ErrorResponse{Error: msg})
}
