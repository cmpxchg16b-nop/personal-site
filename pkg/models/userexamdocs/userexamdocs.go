// Package userexamdocs manages exam documents uploaded by users.
package userexamdocs

import (
	"context"

	pkgmodelsquestion "personal-site/pkg/models/question"
	pkgmodelsuserupload "personal-site/pkg/models/userupload"
)

// ExamDocAssociation is a data-only record of a binding between a user's
// upload and the exam documents derived from it. It is the handle a
// UserExamDocsAssociationManager stores and returns; its purpose is to guide a
// user-aware ExamSource (such as a UserUploadedExamDocumentsManager) to the
// uploads it should load exam documents from for a given user.
type ExamDocAssociation struct {
	// Id is the globally unique identifier of this association.
	Id string

	// UserId is the id of the user who created this association.
	UserId string

	// UploadId identifies the underlying user upload (as known to a
	// UserUploadManager) that this association points at. A consumer reads the
	// exam documents from this upload.
	UploadId string
}

// UserExamDocsAssociationManager manages bindings (associations) between user
// uploads and exam documents. All operations are scoped to a user: associations
// created by one user are not visible or mutable through the user id of another.
// userId, associationId, and uploadId are opaque strings whose meaning is
// defined by the implementation.
type UserExamDocsAssociationManager interface {
	// GetAssociationsByUserId returns every association owned by userId, or an
	// empty slice if the user has none.
	GetAssociationsByUserId(ctx context.Context, userId string) ([]ExamDocAssociation, error)

	// DeleteAssociation removes the association identified by associationId from
	// userId's associations. It returns an error if the association does not exist
	// or does not belong to userId.
	DeleteAssociation(ctx context.Context, userId string, associationId string) error

	// AddAssociation creates a new association owned by userId that points at the
	// given uploadId. It returns an error if the upload does not exist or does not
	// belong to userId.
	AddAssociation(ctx context.Context, userId string, uploadId string) error

	// DereferenceAssociation resolves the association identified by associationId
	// (owned by userId) to the Exam document it refers to, loading and returning
	// that exam. It returns an error if the association does not exist, does not
	// belong to userId, or the underlying upload cannot be loaded as an exam.
	DereferenceAssociation(ctx context.Context, userId string, associationId string) (*pkgmodelsquestion.Exam, error)
}

// UserUploadedExamDocumentsManager manages the lifecycle of exam documents
// that users upload. It implements question.ExamSource by adapting the
// user-scoped uploads stored in a UserUploadManager into exam source entries.
type UserUploadedExamDocumentsManager struct {
	userUploadManager pkgmodelsuserupload.UserUploadManager
	associationMgr    UserExamDocsAssociationManager
}
