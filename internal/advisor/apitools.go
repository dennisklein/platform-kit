package advisor

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/dennisklein/platform-kit/internal/api"
	"github.com/dennisklein/platform-kit/internal/change"
	"github.com/dennisklein/platform-kit/internal/inproc"
	"github.com/dennisklein/platform-kit/internal/llm"
)

// APITools implements Tools over the bench API (in-process, as the caller).
type APITools struct {
	API   *inproc.Client
	Index llm.Index
}

func (t *APITools) get(ctx context.Context, path string, q url.Values, out any) error {
	return t.API.JSON(ctx, http.MethodGet, path, q, nil, out)
}

func (t *APITools) Change(ctx context.Context, id string) (*api.Change, error) {
	var out api.Change
	return &out, t.get(ctx, "/changes/"+inproc.Seg(id), nil, &out)
}

func (t *APITools) Changes(ctx context.Context) ([]api.ChangeSummary, error) {
	var out api.ChangeList
	if err := t.get(ctx, "/changes", nil, &out); err != nil {
		return nil, err
	}
	return out.Changes, nil
}

func (t *APITools) Impact(ctx context.Context, id string) (*api.Impact, error) {
	var out api.Impact
	return &out, t.get(ctx, "/changes/"+inproc.Seg(id)+"/impact", nil, &out)
}

func (t *APITools) Diff(ctx context.Context, id string) (*api.Diff, error) {
	var out api.Diff
	return &out, t.get(ctx, "/changes/"+inproc.Seg(id)+"/diff", url.Values{"view": {"source"}}, &out)
}

func (t *APITools) Status(ctx context.Context, id string) (*api.WorktreeStatus, error) {
	var out api.WorktreeStatus
	return &out, t.get(ctx, "/changes/"+inproc.Seg(id)+"/status", nil, &out)
}

func (t *APITools) Problems(ctx context.Context, id string) ([]api.Problem, error) {
	var out api.ProblemList
	if err := t.get(ctx, "/changes/"+inproc.Seg(id)+"/problems", nil, &out); err != nil {
		return nil, err
	}
	return out.Problems, nil
}

func (t *APITools) Tree(ctx context.Context, ref string) ([]api.FileNode, error) {
	var out api.FileTree
	path := "/refs/" + inproc.Seg(ref) + "/tree"
	if change.ValidID(ref) {
		path = "/changes/" + inproc.Seg(ref) + "/tree"
	}
	if err := t.get(ctx, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Entries, nil
}

func (t *APITools) ReadFile(ctx context.Context, ref, p string) (*api.FileContent, error) {
	var out api.FileContent
	q := url.Values{"path": {p}}
	if change.ValidID(ref) {
		return &out, t.get(ctx, "/changes/"+inproc.Seg(ref)+"/file", q, &out)
	}
	if ref == "" {
		ref = "main"
	}
	return &out, t.get(ctx, "/refs/"+inproc.Seg(ref)+"/file", q, &out)
}

func (t *APITools) WriteFile(ctx context.Context, changeID, p, content string) (*api.WriteFileResult, error) {
	var out api.WriteFileResult
	err := t.API.JSON(ctx, http.MethodPut, "/changes/"+inproc.Seg(changeID)+"/file", url.Values{"path": {p}},
		api.WriteFileRequest{Content: content}, &out)
	return &out, err
}

func (t *APITools) History(ctx context.Context, p api.ListHistoryParams) ([]api.HistoryEntry, error) {
	q := url.Values{}
	for k, v := range map[string]string{"q": p.Q, "entity": p.Entity, "path": p.Path, "author": p.Author, "kind": p.Kind} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	var out api.HistoryList
	if err := t.get(ctx, "/history", q, &out); err != nil {
		return nil, err
	}
	return out.Entries, nil
}

func (t *APITools) HistoryEntry(ctx context.Context, sha string) (*api.HistoryDetail, error) {
	var out api.HistoryDetail
	return &out, t.get(ctx, "/history/"+inproc.Seg(sha), nil, &out)
}

func (t *APITools) LabRuns(ctx context.Context, changeID string) ([]api.LabRun, error) {
	var out api.LabList
	if err := t.get(ctx, "/labs", url.Values{"changeId": {changeID}}, &out); err != nil {
		return nil, err
	}
	runs := out.Runs[:0:0]
	for _, r := range out.Runs {
		if r.ChangeId == "" || r.ChangeId == changeID {
			runs = append(runs, r)
		}
	}
	return runs, nil
}

func (t *APITools) Entity(ctx context.Context, kind, name, ref string) (*api.Entity, error) {
	var out api.Entity
	q := url.Values{}
	if ref != "" {
		q.Set("ref", ref)
	}
	return &out, t.get(ctx, "/entities/"+inproc.Seg(kind)+"/"+inproc.Seg(name), q, &out)
}

func (t *APITools) Search(ctx context.Context, q string, limit int) ([]llm.Hit, error) {
	if t.Index == nil {
		return nil, nil
	}
	return t.Index.Search(ctx, q, limit)
}
