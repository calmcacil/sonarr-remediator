// fake-sonarr is a disposable acceptance fixture, excluded from production images.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "stats" {
		r, e := http.Get("http://127.0.0.1:8989/stats")
		if e != nil {
			os.Exit(1)
		}
		defer r.Body.Close()
		var v map[string]int
		if json.NewDecoder(r.Body).Decode(&v) != nil {
			os.Exit(1)
		}
		fmt.Println(v["mutations"])
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "delay-preview" {
		r, e := http.Post("http://127.0.0.1:8989/fixture/preview-delay", "application/json", nil)
		if e != nil {
			os.Exit(1)
		}
		r.Body.Close()
		if r.StatusCode != 204 {
			os.Exit(1)
		}
		return
	}
	var mu sync.Mutex
	mutations, scans := 0, 0
	var imported time.Time
	var delayPreview bool
	quality := map[string]any{"quality": map[string]any{"id": 3}, "revision": map[string]any{"version": 1, "real": 0, "isRepack": false}}
	series := map[string]any{"id": 10, "qualityProfileId": 20, "monitored": true, "seriesType": "standard"}
	episode := map[string]any{"id": 100, "seriesId": 10, "episodeFileId": 0, "hasFile": false, "monitored": true}
	row := map[string]any{"id": 1, "downloadId": "fake-download", "seriesId": 10, "episodeId": 100, "downloadClient": "fake-client", "title": "fake-release", "outputPath": "/fake/file.mkv", "status": "completed", "trackedDownloadState": "importBlocked"}
	preview := map[string]any{"id": 1, "path": "/fake/file.mkv", "downloadId": "fake-download", "series": series, "episodes": []any{episode}, "quality": quality, "customFormats": []any{}, "customFormatScore": 0, "rejections": []any{}, "languages": []any{map[string]any{"id": 1}}, "indexerFlags": 0, "releaseType": "singleEpisode"}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		var v any
		if r.Method == "POST" && r.URL.Path == "/fixture/preview-delay" {
			delayPreview = true
			w.WriteHeader(204)
			return
		}
		if r.Method != "GET" {
			mutations++
			var command struct {
				Name string `json:"name"`
			}
			if r.Method == "POST" && r.URL.Path == "/api/v3/command" && json.NewDecoder(r.Body).Decode(&command) == nil && command.Name == "ManualImport" {
				imported = time.Now().UTC().Truncate(time.Second)
				episode["episodeFileId"], episode["hasFile"] = 200, true
				json.NewEncoder(w).Encode(map[string]any{"id": mutations, "name": "ManualImport", "status": "completed"})
			} else {
				w.WriteHeader(500)
				json.NewEncoder(w).Encode(map[string]string{"error": "unexpected fixture mutation"})
			}
			return
		}
		switch r.URL.Path {
		case "/stats":
			v = map[string]int{"mutations": mutations, "scans": scans}
		case "/api/v3/system/status":
			v = map[string]string{"version": "4.0.20.3014"}
		case "/api/v3/queue":
			scans++
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			v = map[string]any{"page": page, "pageSize": 100, "totalRecords": 1, "records": []any{row}}
		case "/api/v3/manualimport":
			if delayPreview {
				delayPreview = false
				select {
				case <-time.After(2 * time.Second):
				case <-r.Context().Done():
					return
				}
			}
			v = []any{preview}
		case "/api/v3/series/10":
			v = series
		case "/api/v3/episode/100":
			v = episode
		case "/api/v3/episodefile/200":
			v = map[string]any{"id": 200, "seriesId": 10, "path": "/library/fake.mkv", "quality": quality, "customFormats": []any{}, "customFormatScore": 0}
		case "/api/v3/command/1":
			v = map[string]any{"id": 1, "name": "ManualImport", "status": "completed"}
		case "/api/v3/qualityprofile/20":
			v = map[string]any{"id": 20, "upgradeAllowed": true, "cutoff": 3, "cutoffFormatScore": 100, "minFormatScore": 0, "minUpgradeFormatScore": 1, "items": []any{map[string]any{"quality": map[string]any{"id": 3}, "allowed": true, "items": []any{}}}, "formatItems": []any{}}
		case "/api/v3/config/mediamanagement":
			v = map[string]string{"downloadPropersAndRepacks": "doNotPrefer"}
		case "/api/v3/history":
			records := []any{}
			if !imported.IsZero() {
				records = append(records, map[string]any{"id": 1, "downloadId": "fake-download", "seriesId": 10, "episodeId": 100, "date": imported, "eventType": "downloadFolderImported", "data": map[string]string{"droppedPath": "/fake/file.mkv", "importedPath": "/library/fake.mkv"}})
			}
			v = map[string]any{"page": 1, "pageSize": 100, "totalRecords": len(records), "records": records}
		default:
			w.WriteHeader(404)
			v = map[string]string{"error": "unknown fixture route"}
		}
		json.NewEncoder(w).Encode(v)
	})
	if e := http.ListenAndServe(":8989", nil); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
