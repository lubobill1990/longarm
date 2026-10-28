package hub

import (
	"net/http"
	"sort"

	"github.com/lubobill1990/longarm/internal/hub/store"
)

// The release records, public like the files they describe
// (GET /releases/<version>/<file>): what the LongarmUpdate service
// (package updatesvc) checks a package against - name, sha256, size -
// before it verifies the signature with the key built into it.
//
//	GET /releases/<version>/release.json that version's record (the latest
//	                                     stable: GET /releases/latest.json, updatenotice.go)

type releaseFileJSON struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Signed bool   `json:"signed,omitempty"`
	OS     string `json:"os,omitempty"`
	Arch   string `json:"arch,omitempty"`
}

type releaseJSON struct {
	Version string            `json:"version"`
	Channel string            `json:"channel,omitempty"`
	Files   []releaseFileJSON `json:"files"`
}

func releaseRecord(version string, files []store.Release) releaseJSON {
	out := releaseJSON{Version: version, Files: []releaseFileJSON{}}
	for _, r := range files {
		out.Channel = r.Channel
		out.Files = append(out.Files, releaseFileJSON{Name: r.File, SHA256: r.SHA256, Size: r.Size, Signed: len(r.Sig) > 0, OS: r.OS, Arch: r.Arch})
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Name < out.Files[j].Name })
	return out
}

func (h *Hub) releaseVersionJSON(w http.ResponseWriter, r *http.Request) {
	v := r.PathValue("version")
	if !versionRE.MatchString(v) {
		http.NotFound(w, r)
		return
	}
	files, err := h.releaseFiles(v)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(files) == 0 {
		httpError(w, http.StatusNotFound, "no release "+v)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, releaseRecord(v, files))
}
