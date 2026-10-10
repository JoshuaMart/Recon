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

// maxImportBytes bounds an imported stream. A scanner delivering, not a person
// typing, so it is the report's bound. The largest file seen is under a megabyte,
// and the room above is for a perimeter; the asset count below bounds the work.
const maxImportBytes = 64 << 20

// maxImportAssets bounds what one call may write. An import uses the same
// statement as every asset, one round trip each (refusing a second implementation
// of classification and scheduling), and this number pays for that. Refused, not
// truncated: silently keeping the first ten thousand would report a smaller
// perimeter than the caller handed over.
const maxImportAssets = 10_000

// ownsProgram answers the same question as owns, in its own transaction, so the
// check can precede reading the body. The write's transaction asks again; this
// answer is not trusted for it.
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

// ImportBBOT turns a scan run outside this platform into inventory. It sits on
// Programs beside the assets form: same act, same action, same two checks, and a
// second place deciding who may touch which programme is a second place to be wrong.
func (h *Programs) ImportBBOT(w http.ResponseWriter, r *http.Request, principal auth.Principal) {
	ctx := r.Context()

	programID, ok := pathUUID(w, r, "program")
	if !ok {
		return
	}

	// The programme is checked before the body is read, so a caller holding
	// manage_scope elsewhere cannot spend sixty-four megabytes of decoding against a
	// programme it cannot access. One short transaction around one read is cheaper.
	if !h.ownsProgram(ctx, w, principal, programID) {
		return
	}

	scan, err := bbot.Parse(http.MaxBytesReader(w, r.Body, maxImportBytes))
	if err != nil {
		var refusal *bbot.Error
		if errors.As(err, &refusal) {
			// The file's shape is the caller's, and naming it makes the endpoint usable:
			// "malformed" on valid JSON would not teach that the container was wrong.
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

	// Again, inside the writing transaction. The check above bounds what a stranger
	// can ask for; this one decides, as the programme could be archived or handed over
	// in between.
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
