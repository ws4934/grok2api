package audit

import (
	"bytes"
	"encoding/json"

	"github.com/chenyme/grok2api/backend/internal/pkg/requestparams"
)

// JSON is text on the admin wire so seeds and IDs outside JavaScript's safe
// integer range are displayed and copied without rounding.
type requestParameterSnapshotResponse struct {
	JSON      string `json:"json"`
	Truncated bool   `json:"truncated"`
}
type requestParametersResponse struct {
	Client    *requestParameterSnapshotResponse `json:"client,omitempty"`
	Effective *requestParameterSnapshotResponse `json:"effective,omitempty"`
	Upstream  *requestParameterSnapshotResponse `json:"upstream,omitempty"`
}

func newRequestParametersResponse(parameters *requestparams.Parameters) *requestParametersResponse {
	if parameters == nil {
		return nil
	}
	convert := func(s *requestparams.Snapshot) *requestParameterSnapshotResponse {
		if s == nil {
			return nil
		}
		var pretty bytes.Buffer
		if json.Indent(&pretty, s.Values, "", "  ") != nil {
			return nil
		}
		return &requestParameterSnapshotResponse{JSON: pretty.String(), Truncated: s.Truncated}
	}
	d := parameters.Data()
	return &requestParametersResponse{Client: convert(d.Client), Effective: convert(d.Effective), Upstream: convert(d.Upstream)}
}
