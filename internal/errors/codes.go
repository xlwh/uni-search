package errors

// ES-aligned error type constants; the REST layer reports these verbatim
// as error.type.
const (
	TypeIllegalArgument       = "illegal_argument"
	TypeIndexNotFound         = "index_not_found_exception"
	TypeDocumentParsing       = "document_parsing_exception"
	TypeVersionConflict       = "version_conflict_engine_exception"
	TypeUnsupportedCapability = "unsupported_capability"
	TypeInternal              = "internal_error"
)

// HTTP status codes used by the error registry. Raw literals keep this
// package free of any dependency, including net/http.
const (
	statusBadRequest    = 400
	statusNotFound      = 404
	statusConflict      = 409
	statusInternalError = 500
)

// NewIllegalArgument returns a 400 illegal_argument error (bad parameter
// values, malformed requests).
func NewIllegalArgument(reason string) *APIError {
	return &APIError{Status: statusBadRequest, Type: TypeIllegalArgument, Reason: reason}
}

// NewIndexNotFound returns a 404 index_not_found_exception for the given
// index.
func NewIndexNotFound(index string) *APIError {
	return &APIError{
		Status: statusNotFound,
		Type:   TypeIndexNotFound,
		Reason: "no such index [" + index + "]",
		Index:  index,
	}
}

// NewDocumentParsing returns a 400 document_parsing_exception (malformed
// document source, mapping violations).
func NewDocumentParsing(reason string) *APIError {
	return &APIError{Status: statusBadRequest, Type: TypeDocumentParsing, Reason: reason}
}

// NewVersionConflict returns a 409 version_conflict_engine_exception (e.g.
// if_seq_no optimistic-locking failures).
func NewVersionConflict(index, reason string) *APIError {
	return &APIError{Status: statusConflict, Type: TypeVersionConflict, Reason: reason, Index: index}
}

// NewUnsupported returns an error for a recognized-but-unimplemented
// capability. Spec R1 requires these to surface as HTTP 400 (ES parity for
// unsupported parameters), not 501; Unsupported carries the migration
// guidance (feature, planned milestone, workaround).
func NewUnsupported(feature, milestone, workaround string) *APIError {
	return &APIError{
		Status: statusBadRequest,
		Type:   TypeUnsupportedCapability,
		Reason: feature + " is not supported yet; planned for " + milestone,
		Unsupported: &UnsupportedInfo{
			Feature:    feature,
			Milestone:  milestone,
			Workaround: workaround,
		},
	}
}

// NewInternal returns a 500 internal error. Stack details must be logged
// server-side (with trace_id) and never leaked through the response body.
func NewInternal(reason string) *APIError {
	return &APIError{Status: statusInternalError, Type: TypeInternal, Reason: reason}
}
