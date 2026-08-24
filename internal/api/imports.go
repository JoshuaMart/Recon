package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/JoshuaMart/recon/internal/auth"
	"github.com/JoshuaMart/recon/internal/bbot"
	"github.com/JoshuaMart/recon/internal/ingest"
	"github.com/JoshuaMart/recon/internal/store/sqlcgen"
)

// maxImportBytes bounds an imported stream.
//
// A scanner delivering rather than a person typing, so it is the report's bound
// and not the assets form's. The largest file anybody has produced here is
// under a megabyte, and the room above that is for a perimeter, not for a
// mistake: what actually bounds the work is the asset count below.
const maxImportBytes = 64 << 20

// maxImportAssets bounds what one call may write.
//
// An import goes through the same statement as every other asset, one round
// trip each, which is a deliberate refusal to keep a second implementation of
// classification and scheduling. That choice is what this number pays for. It
// is refused rather than truncated: an import that silently kept the first ten
// thousand assets of a file would report a perimeter smaller than the one the
// caller handed over, and nothing would say so.
const maxImportAssets = 10_000

// ownsProgram answers the same question as owns, on a transaction of its own.
//
// It exists so the check can happen before the body is read. Its answer is not
// trusted for the write: the transaction that writes asks again.
func (h *Programs) ownsProgram(
	ctx context.Context, w http.ResponseWriter, principal auth.Principal, programID uuid.UUID,
) bool {
	tx, err := h.db.Begin(ctx, principal.OrgID)
	if err != nil {
		h.unavailable(ctx, w, "begin failed", err)
		return false
	}
	defer func() { _ = tx.Rollback(ctx) }()

	return h.owns(ctx, w, sqlcgen.New(tx), principal, programID)
}

// ImportBBOT turns a scan run outside this platform into inventory.
//
// It sits on Programs beside the assets form rather than in a handler of its
// own, because it is the same act under the same action and it needs the same
// two checks. A second place deciding who may touch which programme is a second
// place to get it wrong.
func (h *Programs) ImportBBOT(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()

	programID, ok := pathUUID(w, r, "program")
	if !ok {
		return
	}

	// The programme is checked before the body is read, so a caller holding
	// manage_scope somewhere cannot spend sixty four megabytes of decoding
	// against a programme it has no access to. It costs one transaction that is
	// opened and closed around a single read, which is the cheaper of the two.
	if !h.ownsProgram(ctx, w, principal, programID) {
		return
	}

	scan, err := bbot.Parse(http.MaxBytesReader(w, r.Body, maxImportBytes))
	if err != nil {
		var refusal *bbot.Error
		if errors.As(err, &refusal) {
			// The file's shape is the caller's, and naming it is what makes the
			// endpoint usable: "malformed" on a body that is valid JSON teaches
			// nobody that the container was wrong rather than the content.
			fail(w, http.StatusBadRequest, "malformed", refusal.Error())
			return
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, http.StatusRequestEntityTooLarge, "too_large",
				fmt.Sprintf("the stream exceeds the bound of %d bytes", maxImportBytes))
			return
		}
		fail(w, http.StatusBadRequest, "malformed", "the body is not a BBOT event stream")
		return
	}

	if count := scan.Assets(); count > maxImportAssets {
		fail(w, http.StatusRequestEntityTooLarge, "too_many",
			fmt.Sprintf("%d assets exceeds the bound of %d, and an import is refused rather than truncated",
				count, maxImportAssets))
		return
	}

	tx, err := h.db.Begin(ctx, principal.OrgID)
	if err != nil {
		h.unavailable(ctx, w, "begin failed", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)

	// Again, inside the transaction that writes. The check above bounds the
	// work a stranger can ask for; this one is the one that decides, because
	// between the two the programme could have been archived or handed over.
	if !h.owns(ctx, w, q, principal, programID) {
		return
	}

	set, err := compileScope(ctx, q, programID, h.now())
	if err != nil {
		h.unavailable(ctx, w, "read perimeter failed", err)
		return
	}

	imported, err := h.ingestor.Import(ctx, q, ingest.Run{
		ID:        uuid.New(),
		OrgID:     principal.OrgID,
		ProgramID: programID,
		Kind:      "import",
	}, set, scan)
	if err != nil {
		h.unavailable(ctx, w, "import failed", err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		h.unavailable(ctx, w, "commit failed", err)
		return
	}

	h.log.InfoContext(ctx, "scan imported",
		"program", programID, "scan", imported.Scan.ID,
		"created", imported.Assets.Created, "existing", imported.Assets.Existing,
		"scheduled", imported.Assets.Scheduled, "refused", len(imported.Refused))
	writeJSON(w, http.StatusOK, imported)
}
