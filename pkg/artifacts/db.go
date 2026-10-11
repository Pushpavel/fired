package artifacts

import (
	"encoding/json"
	"time"
)

// ---- the db
//
// Every artifact has a small document store, its db: JSON objects at
// slash-separated paths, like Firestore. A path with an even number of
// segments is a document ("stats/today"), an odd one a collection
// ("stats", "stats/today/hours"); a collection is the documents directly
// under it. The job writes it, the page reads it:
//
//   - the API token (your backend, agents) and the artifact's owner read
//     and write everything;
//   - a running job reads and writes everything, with $FIRED_DB_TOKEN;
//   - everyone else who can view the artifact reads it, except the
//     collection PrivateCollection, which is the owner's and the job's.

// PrivateCollection holds documents only the owner and the job see, such
// as the job's working state and raw data: a shared artifact exposes the
// rest of its db to its viewers.
const PrivateCollection = "_private"

// Doc is one document.
type Doc struct {
	Path string          `json:"path"`
	Data json.RawMessage `json:"data,omitempty"`
	// Rev increases with every write; pass it back to write only if the
	// document is unchanged since (If-Match, or DBWrite.Rev).
	Rev       int64     `json:"rev"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Deleted is set in a batch result for a delete.
	Deleted bool `json:"deleted,omitempty"`
}

// DocList is a page of documents. Next, when set, is passed as ?after= for
// the next page.
type DocList struct {
	Docs []Doc  `json:"docs"`
	Next string `json:"next,omitempty"`
}

// DBWrite is one write of a batch.
type DBWrite struct {
	// Op is DBSet (replace the document), DBUpdate (JSON merge patch,
	// creating it if missing) or DBDelete.
	Op   string          `json:"op"`
	Path string          `json:"path"`
	Data json.RawMessage `json:"data,omitempty"`
	// Rev, when set, makes the batch fail unless the document is at this
	// revision (-1: unless it doesn't exist).
	Rev int64 `json:"rev,omitempty"`
}

const (
	DBSet    = "set"
	DBUpdate = "update"
	DBDelete = "delete"
)

// DBBatch applies writes in order, all or none.
type DBBatch struct {
	Writes []DBWrite `json:"writes"`
}

type DBBatchResult struct {
	Docs []Doc `json:"docs"`
}

// DocQuery selects documents of a collection.
type DocQuery struct {
	// Where are filters "field op value", all of which must hold. field is
	// a top-level key or a dotted path (a.b), op one of == != < <= > >=,
	// value JSON (a bare word is a string): "done==false", "n>=3",
	// "owner==alice".
	Where []string
	// Order is a field, or -field for descending; documents without it come
	// last. Default: by path.
	Order string
	Limit int
	After string
}
