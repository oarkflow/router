package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/sergi/go-diff/diffmatchpatch"
)

var watchPaths = []string{"./configs"}

type FileVersion struct {
	Timestamp time.Time `json:"timestamp"`
	Content   string    `json:"content"`
	Diff      string    `json:"diff,omitempty"`
}

type Commit struct {
	ID        int                    `json:"id"`
	Timestamp time.Time              `json:"timestamp"`
	Message   string                 `json:"message"`
	Files     map[string]FileVersion `json:"files"`
}

type VersionGroup struct {
	ID            int                    `json:"id"`
	Tag           string                 `json:"tag,omitempty"`
	CommitMessage string                 `json:"commitMessage,omitempty"`
	Timestamp     time.Time              `json:"timestamp"`
	Files         map[string]FileVersion `json:"files"`
}

type VersionManager struct {
	sync.Mutex
	latestFiles     map[string]string
	fileVersions    map[string][]FileVersion
	commits         []Commit
	nextCommitID    int
	versions        []VersionGroup
	nextVerID       int
	committedFiles  map[string]string
	deployedVersion *VersionGroup
}

func NewVersionManager() *VersionManager {
	return &VersionManager{
		latestFiles:     make(map[string]string),
		fileVersions:    make(map[string][]FileVersion),
		commits:         []Commit{},
		versions:        []VersionGroup{},
		committedFiles:  make(map[string]string),
		deployedVersion: nil,
		nextCommitID:    1,
		nextVerID:       1,
	}
}

var versionManager = NewVersionManager()

func formatDiff(diffs []diffmatchpatch.Diff) string {
	var result strings.Builder
	for _, d := range diffs {
		lines := strings.Split(d.Text, "\n")
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			for _, line := range lines {
				if line != "" {
					result.WriteString(" " + line + "\n")
				}
			}
		case diffmatchpatch.DiffInsert:
			for _, line := range lines {
				if line != "" {
					result.WriteString("+" + line + "\n")
				}
			}
		case diffmatchpatch.DiffDelete:
			for _, line := range lines {
				if line != "" {
					result.WriteString("-" + line + "\n")
				}
			}
		}
	}
	return result.String()
}

func threeWayMerge(base, v1, v2 string) (string, bool) {
	if v1 == v2 {
		return v1, false
	}
	if v1 == base {
		return v2, false
	}
	if v2 == base {
		return v1, false
	}
	return "", true
}

func deployVersion(ver VersionGroup) error {
	for srcPath, fileVersion := range ver.Files {
		relPath := strings.TrimPrefix(srcPath, "configs/")
		destPath := filepath.Join("Prod", relPath)
		destDir := filepath.Dir(destPath)
		if err := os.MkdirAll(destDir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %v", destDir, err)
		}
		if err := os.WriteFile(destPath, []byte(fileVersion.Content), 0644); err != nil {
			return fmt.Errorf("failed to write file %s: %v", destPath, err)
		}
		log.Printf("Deployed %s", destPath)
	}
	return nil
}

func (vm *VersionManager) UpdateFile(path, content string) {
	vm.Lock()
	defer vm.Unlock()
	version := FileVersion{Timestamp: time.Now(), Content: content}
	vm.latestFiles[path] = content
	vm.fileVersions[path] = append(vm.fileVersions[path], version)
	log.Printf("File updated: %s", path)
}

func (vm *VersionManager) GetChanges() map[string]string {
	vm.Lock()
	defer vm.Unlock()
	changes := make(map[string]string)
	dmp := diffmatchpatch.New()
	for file, versions := range vm.fileVersions {
		baseline := ""
		if c, ok := vm.committedFiles[file]; ok {
			baseline = strings.TrimSpace(c)
		}
		latest := strings.TrimSpace(versions[len(versions)-1].Content)
		log.Printf("Comparing file '%s': baseline='%s', latest='%s'", file, baseline, latest)
		if latest == baseline {
			continue
		}
		diffs := dmp.DiffMain(baseline, latest, false)
		dmp.DiffCleanupSemantic(diffs)
		diffText := formatDiff(diffs)
		if strings.TrimSpace(diffText) != "" {
			changes[file] = diffText
		}
	}
	return changes
}

func (vm *VersionManager) CreateCommit(selectedFiles []string, message string) Commit {
	vm.Lock()
	defer vm.Unlock()
	dmp := diffmatchpatch.New()
	commit := Commit{
		ID:        vm.nextCommitID,
		Timestamp: time.Now(),
		Message:   message,
		Files:     make(map[string]FileVersion),
	}
	for _, file := range selectedFiles {
		if versions, exists := vm.fileVersions[file]; exists && len(versions) > 0 {
			baseline := ""
			if c, ok := vm.committedFiles[file]; ok {
				baseline = strings.TrimSpace(c)
			}
			current := strings.TrimSpace(versions[len(versions)-1].Content)
			diffs := dmp.DiffMain(baseline, current, false)
			dmp.DiffCleanupSemantic(diffs)
			diffText := formatDiff(diffs)
			fv := FileVersion{
				Timestamp: time.Now(),
				Content:   current,
				Diff:      diffText,
			}
			commit.Files[file] = fv
			vm.committedFiles[file] = current
			vm.fileVersions[file] = []FileVersion{{Timestamp: time.Now(), Content: current}}
		}
	}
	vm.commits = append(vm.commits, commit)
	vm.nextCommitID++
	log.Printf("Created commit %d: %s", commit.ID, message)
	return commit
}

func (vm *VersionManager) MergeCommits(tag string) (VersionGroup, error) {
	vm.Lock()
	defer vm.Unlock()
	var mergedMsg []string
	fileCommits := make(map[string][]FileVersion)
	var conflicts []string
	for _, commit := range vm.commits {
		mergedMsg = append(mergedMsg, fmt.Sprintf("Commit %d: %s", commit.ID, commit.Message))
		for file, version := range commit.Files {
			fileCommits[file] = append(fileCommits[file], version)
		}
	}
	mergedFiles := make(map[string]FileVersion)
	for file, changes := range fileCommits {
		base := ""
		if b, ok := vm.committedFiles[file]; ok {
			base = strings.TrimSpace(b)
		}
		merged := base
		conflictOccurred := false
		for _, candidate := range changes {
			newMerged, conflict := threeWayMerge(base, merged, strings.TrimSpace(candidate.Content))
			if conflict {
				conflictOccurred = true
				break
			}
			merged = newMerged
		}
		if conflictOccurred {
			conflicts = append(conflicts, file)
		} else {
			dmp := diffmatchpatch.New()
			finalDiff := formatDiff(dmp.DiffMain(base, merged, false))
			mergedFiles[file] = FileVersion{
				Timestamp: time.Now(),
				Content:   merged,
				Diff:      finalDiff,
			}
		}
	}
	if len(conflicts) > 0 {
		return VersionGroup{}, fmt.Errorf("merge conflict in files: %s", strings.Join(conflicts, ", "))
	}
	mergedVersion := VersionGroup{
		ID:            vm.nextVerID,
		Tag:           tag,
		CommitMessage: strings.Join(mergedMsg, " | "),
		Timestamp:     time.Now(),
		Files:         mergedFiles,
	}
	vm.versions = append(vm.versions, mergedVersion)
	vm.nextVerID++
	vm.commits = []Commit{}
	log.Printf("Created version %d with tag '%s'", mergedVersion.ID, tag)
	return mergedVersion, nil
}

func (vm *VersionManager) MergeSelectedCommits(commitIDs []int, tag string) (VersionGroup, error) {
	vm.Lock()
	defer vm.Unlock()
	var mergedMsg []string
	fileCommits := make(map[string][]FileVersion)
	var conflicts []string
	selectedMap := make(map[int]bool)
	for _, id := range commitIDs {
		selectedMap[id] = true
	}
	var remainingCommits []Commit
	for _, commit := range vm.commits {
		if selectedMap[commit.ID] {
			mergedMsg = append(mergedMsg, fmt.Sprintf("Commit %d: %s", commit.ID, commit.Message))
			for file, fv := range commit.Files {
				fileCommits[file] = append(fileCommits[file], fv)
			}
		} else {
			remainingCommits = append(remainingCommits, commit)
		}
	}
	mergedFiles := make(map[string]FileVersion)
	for file, changes := range fileCommits {
		base := ""
		if b, ok := vm.committedFiles[file]; ok {
			base = strings.TrimSpace(b)
		}
		merged := base
		conflictOccurred := false
		for _, candidate := range changes {
			newMerged, conflict := threeWayMerge(base, merged, strings.TrimSpace(candidate.Content))
			if conflict {
				conflictOccurred = true
				break
			}
			merged = newMerged
		}
		if conflictOccurred {
			conflicts = append(conflicts, file)
		} else {
			dmp := diffmatchpatch.New()
			finalDiff := formatDiff(dmp.DiffMain(base, merged, false))
			mergedFiles[file] = FileVersion{
				Timestamp: time.Now(),
				Content:   merged,
				Diff:      finalDiff,
			}
		}
	}
	if len(conflicts) > 0 {
		return VersionGroup{}, fmt.Errorf("merge conflict in files: %s", strings.Join(conflicts, ", "))
	}
	mergedVersion := VersionGroup{
		ID:            vm.nextVerID,
		Tag:           tag,
		CommitMessage: strings.Join(mergedMsg, " | "),
		Timestamp:     time.Now(),
		Files:         mergedFiles,
	}
	vm.versions = append(vm.versions, mergedVersion)
	vm.nextVerID++
	vm.commits = remainingCommits
	log.Printf("Created version %d with tag '%s' merging commits: %v", mergedVersion.ID, tag, commitIDs)
	return mergedVersion, nil
}

func (vm *VersionManager) RevertPendingCommits() {
	vm.Lock()
	defer vm.Unlock()
	vm.commits = []Commit{}
	log.Println("Pending commits reverted.")
}

func (vm *VersionManager) AbortMerge() {
	vm.Lock()
	defer vm.Unlock()
	log.Println("Merge aborted. No changes applied.")
}

func (vm *VersionManager) GetDiff(path, newContent string) string {
	vm.Lock()
	defer vm.Unlock()
	baseline := ""
	if content, ok := vm.latestFiles[path]; ok {
		baseline = content
	}
	dmp := diffmatchpatch.New()
	diffs := dmp.DiffMain(baseline, newContent, false)
	dmp.DiffCleanupSemantic(diffs)
	return formatDiff(diffs)
}

func watchFiles(paths []string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		_ = watcher.Close()
	}()
	for _, root := range paths {
		err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if strings.HasSuffix(info.Name(), "~") {
				return nil
			}
			if info.IsDir() {
				log.Println("Watching:", path)
				return watcher.Add(path)
			}
			return nil
		})
		if err != nil {
			log.Fatal(err)
		}
	}
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if strings.HasSuffix(event.Name, "~") {
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				data, err := os.ReadFile(event.Name)
				if err != nil {
					log.Printf("Error reading %s: %v", event.Name, err)
					continue
				}
				content := string(data)
				diff := versionManager.GetDiff(event.Name, content)
				log.Printf("Change on %s\nDiff:\n%s", event.Name, diff)
				versionManager.UpdateFile(event.Name, content)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Println("Watcher error:", err)
		}
	}
}

func handleChanges(w http.ResponseWriter, r *http.Request) {
	changes := versionManager.GetChanges()
	_ = json.NewEncoder(w).Encode(changes)
}

type CommitPayload struct {
	Message string   `json:"message"`
	Files   []string `json:"files"`
}

func handleCommit(w http.ResponseWriter, r *http.Request) {
	var payload CommitPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	commit := versionManager.CreateCommit(payload.Files, payload.Message)
	_ = json.NewEncoder(w).Encode(commit)
}

func handleGetCommits(w http.ResponseWriter, r *http.Request) {
	versionManager.Lock()
	defer versionManager.Unlock()
	_ = json.NewEncoder(w).Encode(versionManager.commits)
}

type VersionPayload struct {
	Tag string `json:"tag"`
}

func handleCreateVersion(w http.ResponseWriter, r *http.Request) {
	var payload VersionPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	ver, err := versionManager.MergeCommits(payload.Tag)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	_ = json.NewEncoder(w).Encode(ver)
}

type MergeVersionPayload struct {
	Tag       string `json:"tag"`
	CommitIDs []int  `json:"commit_ids"`
}

func handleMergeSelectedCommits(w http.ResponseWriter, r *http.Request) {
	var payload MergeVersionPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	ver, err := versionManager.MergeSelectedCommits(payload.CommitIDs, payload.Tag)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	_ = json.NewEncoder(w).Encode(ver)
}

func handleRevertCommits(w http.ResponseWriter, r *http.Request) {
	versionManager.RevertPendingCommits()
	_, _ = w.Write([]byte("Pending commits reverted."))
}

func handleAbortMerge(w http.ResponseWriter, r *http.Request) {
	versionManager.AbortMerge()
	_, _ = w.Write([]byte("Merge aborted."))
}

func handleGetVersions(w http.ResponseWriter, r *http.Request) {
	versionManager.Lock()
	defer versionManager.Unlock()
	_ = json.NewEncoder(w).Encode(versionManager.versions)
}

type SwitchVersionPayload struct {
	VersionID int `json:"version_id"`
}

func handleSwitchVersion(w http.ResponseWriter, r *http.Request) {
	var payload SwitchVersionPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	versionManager.Lock()
	defer versionManager.Unlock()
	var selected *VersionGroup
	for _, ver := range versionManager.versions {
		if ver.ID == payload.VersionID {
			selected = &ver
			break
		}
	}
	if selected == nil {
		http.Error(w, "Version not found", http.StatusNotFound)
		return
	}
	if err := deployVersion(*selected); err != nil {
		http.Error(w, fmt.Sprintf("Failed to deploy version: %v", err), http.StatusInternalServerError)
		return
	}
	versionManager.deployedVersion = selected
	log.Printf("Switched to deployed version %d", selected.ID)
	_ = json.NewEncoder(w).Encode(selected)
}

func handleDeployedVersion(w http.ResponseWriter, r *http.Request) {
	versionManager.Lock()
	defer versionManager.Unlock()
	if versionManager.deployedVersion == nil {
		http.Error(w, "No deployed version", http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(versionManager.deployedVersion)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "./static/index.html")
}

func main() {
	go watchFiles(watchPaths)
	http.HandleFunc("/api/changes", handleChanges)
	http.HandleFunc("/api/commit", handleCommit)
	http.HandleFunc("/api/commits", handleGetCommits)
	http.HandleFunc("/api/version", handleCreateVersion)
	http.HandleFunc("/api/version/mergeSelected", handleMergeSelectedCommits)
	http.HandleFunc("/api/version/revert", handleRevertCommits)
	http.HandleFunc("/api/merge/abort", handleAbortMerge)
	http.HandleFunc("/api/versions", handleGetVersions)
	http.HandleFunc("/api/version/switch", handleSwitchVersion)
	http.HandleFunc("/api/deployedVersion", handleDeployedVersion)
	http.HandleFunc("/", handleIndex)
	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))
	addr := ":8080"
	log.Printf("Server starting on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}
