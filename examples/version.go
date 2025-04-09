package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bbolt "go.etcd.io/bbolt"

	"github.com/fsnotify/fsnotify"
	"github.com/sergi/go-diff/diffmatchpatch"
)

const (
	Username = "admin"       // Load from environment/configuration in production
	Password = "supersecret" // Load from environment/configuration in production

	dbFile = "versionmanager.db"
)

var watchPaths = []string{"./configs"}

// --- Entity definitions ---

// FileVersion holds content and diff info. If Deleted is true, then content is empty.
type FileVersion struct {
	Timestamp time.Time `json:"timestamp"`
	Content   string    `json:"content"`
	Diff      string    `json:"diff,omitempty"`
	Deleted   bool      `json:"deleted,omitempty"`
}

// Commit holds a commit with associated file versions and branch info.
type Commit struct {
	ID        int                    `json:"id"`
	Timestamp time.Time              `json:"timestamp"`
	Message   string                 `json:"message"`
	Branch    string                 `json:"branch"`
	Files     map[string]FileVersion `json:"files"`
}

// VersionGroup holds a merged version snapshot.
type VersionGroup struct {
	ID            int                    `json:"id"`
	Tag           string                 `json:"tag,omitempty"`
	CommitMessage string                 `json:"commitMessage,omitempty"`
	Timestamp     time.Time              `json:"timestamp"`
	Branch        string                 `json:"branch"`
	Files         map[string]FileVersion `json:"files"`
}

// --- Persistent Storage using bbolt ---

// Storage encapsulates a bbolt DB instance.
type Storage struct {
	db *bbolt.DB
}

var (
	commitsBucket     = []byte("Commits")
	versionsBucket    = []byte("Versions")
	branchesBucket    = []byte("Branches")
	committedFilesBkt = []byte("CommittedFiles")
	auditLogBucket    = []byte("AuditLog")
)

// NewStorage opens (or creates) the bolt database and ensures that buckets exist.
func NewStorage(dbFile string) (*Storage, error) {
	db, err := bbolt.Open(dbFile, 0600, &bbolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, err
	}
	// Ensure buckets exist.
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, bkt := range [][]byte{commitsBucket, versionsBucket, branchesBucket, committedFilesBkt, auditLogBucket} {
			_, err := tx.CreateBucketIfNotExists(bkt)
			if err != nil {
				return fmt.Errorf("create bucket %s: %v", bkt, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &Storage{db: db}, nil
}

// SaveEntity saves an entity (e.g. commit or version) to a specified bucket using its ID as key.
func (s *Storage) SaveEntity(bucket []byte, id int, entity interface{}) error {
	data, err := json.Marshal(entity)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucket)
		key := []byte(fmt.Sprintf("%d", id))
		return b.Put(key, data)
	})
}

// LoadEntities loads all entities from a bucket and unmarshals them into a slice.
func (s *Storage) LoadEntities(bucket []byte, out interface{}) error {
	return s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucket)
		var list []json.RawMessage
		err := b.ForEach(func(k, v []byte) error {
			list = append(list, v)
			return nil
		})
		if err != nil {
			return err
		}
		data, err := json.Marshal(list)
		if err != nil {
			return err
		}
		return json.Unmarshal(data, out)
	})
}

// SaveCommittedFiles stores the committedFiles map.
func (s *Storage) SaveCommittedFiles(files map[string]string) error {
	data, err := json.Marshal(files)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(committedFilesBkt)
		return b.Put([]byte("base"), data)
	})
}

// LoadCommittedFiles loads the committedFiles map.
func (s *Storage) LoadCommittedFiles() (map[string]string, error) {
	var files map[string]string
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(committedFilesBkt)
		data := b.Get([]byte("base"))
		if data == nil {
			files = make(map[string]string)
			return nil
		}
		return json.Unmarshal(data, &files)
	})
	return files, err
}

// SaveBranch persists the current branch.
func (s *Storage) SaveBranch(branch string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(branchesBucket)
		return b.Put([]byte("current"), []byte(branch))
	})
}

// LoadBranch retrieves the current branch.
func (s *Storage) LoadBranch() (string, error) {
	var branch string
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(branchesBucket)
		data := b.Get([]byte("current"))
		if data == nil {
			branch = "main"
		} else {
			branch = string(data)
		}
		return nil
	})
	return branch, err
}

// AppendAudit appends an audit log entry.
func (s *Storage) AppendAudit(entry string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(auditLogBucket)
		id, _ := b.NextSequence()
		key := []byte(fmt.Sprintf("%d", id))
		return b.Put(key, []byte(entry))
	})
}

// --- Version Manager ---

// VersionManager holds all commit and version data along with a pointer to the storage engine.
type VersionManager struct {
	sync.RWMutex
	latestFiles     map[string]string        // latest known content
	fileVersions    map[string][]FileVersion // history of file versions (per file)
	commits         []Commit                 // pending commits (in current branch)
	nextCommitID    int                      // auto-increment commit id
	versions        []VersionGroup           // merged versions (all branches)
	nextVerID       int                      // auto-increment version group id
	committedFiles  map[string]string        // latest committed file contents (per branch)
	deployedVersion *VersionGroup            // currently deployed version
	currentBranch   string                   // current branch name (e.g. "main", "feature")
	auditLog        []string                 // audit log entries
	storage         *Storage
}

func NewVersionManager(storage *Storage) *VersionManager {
	vm := &VersionManager{
		latestFiles:    make(map[string]string),
		fileVersions:   make(map[string][]FileVersion),
		commits:        []Commit{},
		versions:       []VersionGroup{},
		committedFiles: make(map[string]string),
		nextCommitID:   1,
		nextVerID:      1,
		currentBranch:  "main",
		auditLog:       []string{},
		storage:        storage,
	}
	// Load persisted baseline and branch.
	if base, err := storage.LoadCommittedFiles(); err == nil {
		vm.committedFiles = base
	}
	if branch, err := storage.LoadBranch(); err == nil {
		vm.currentBranch = branch
	}
	// NOTE: Loading commits, versions, and audit log is possible if desired.
	return vm
}

var versionManager *VersionManager

// --- Diff and Merge Functions ---

// formatDiff returns a unified diff string from diffmatchpatch diff results.
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

// mergeFileVersions applies each candidate’s changes sequentially as a patch
// to the currently merged result. If any patch fails to apply, a conflict is declared.
func mergeFileVersions(base string, candidates []string) (string, bool) {
	merged := base
	dmp := diffmatchpatch.New()
	conflictOccurred := false
	for _, candidate := range candidates {
		patches := dmp.PatchMake(merged, candidate)
		result, applied := dmp.PatchApply(patches, merged)
		for _, ok := range applied {
			if !ok {
				conflictOccurred = true
				break
			}
		}
		if conflictOccurred {
			break
		}
		merged = result
	}
	return merged, conflictOccurred
}

// --- Deployment Function ---

// deployVersion writes files to a temporary folder then atomically swaps it with production.
func deployVersion(ver VersionGroup) error {
	tempDir := "Prod_temp"
	if err := os.RemoveAll(tempDir); err != nil {
		return fmt.Errorf("failed to clear temp folder: %v", err)
	}
	// Create temporary directory structure and write files.
	for srcPath, fileVersion := range ver.Files {
		relPath := strings.TrimPrefix(srcPath, "configs/")
		destPath := filepath.Join(tempDir, relPath)
		destDir := filepath.Dir(destPath)
		if err := os.MkdirAll(destDir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %v", destDir, err)
		}
		// Do not write files marked as deleted.
		if fileVersion.Deleted {
			continue
		}
		if err := os.WriteFile(destPath, []byte(fileVersion.Content), 0644); err != nil {
			return fmt.Errorf("failed to write file %s: %v", destPath, err)
		}
		log.Printf("Staged %s", destPath)
	}
	prodDir := "Prod"
	backupDir := "Prod_backup"
	// Backup current production.
	if _, err := os.Stat(prodDir); err == nil {
		os.RemoveAll(backupDir)
		if err := os.Rename(prodDir, backupDir); err != nil {
			return fmt.Errorf("failed to backup production folder: %v", err)
		}
	}
	if err := os.Rename(tempDir, prodDir); err != nil {
		os.Rename(backupDir, prodDir)
		return fmt.Errorf("failed to deploy new version: %v", err)
	}
	os.RemoveAll(backupDir)
	log.Printf("Deployed new version to production.")
	return nil
}

// --- VersionManager Methods ---

// UpdateFile records file changes; if content is empty it is a deletion.
func (vm *VersionManager) UpdateFile(path, content string, deleted bool) {
	vm.Lock()
	defer vm.Unlock()
	version := FileVersion{Timestamp: time.Now(), Content: content, Deleted: deleted}
	vm.latestFiles[path] = content
	vm.fileVersions[path] = append(vm.fileVersions[path], version)
	entry := fmt.Sprintf("%s updated at %s (deleted=%v)", path, time.Now().Format(time.RFC3339), deleted)
	vm.auditLog = append(vm.auditLog, entry)
	// Append audit record.
	_ = vm.storage.AppendAudit(entry)
	log.Printf("File updated: %s (deleted=%v)", path, deleted)
}

// GetChanges computes the diff between the latest file state and the last committed state.
func (vm *VersionManager) GetChanges() map[string]string {
	vm.RLock()
	defer vm.RUnlock()
	changes := make(map[string]string)
	dmp := diffmatchpatch.New()
	for file, versions := range vm.fileVersions {
		baseline := ""
		if c, ok := vm.committedFiles[file]; ok {
			baseline = strings.TrimSpace(c)
		}
		latest := strings.TrimSpace(versions[len(versions)-1].Content)
		if versions[len(versions)-1].Deleted {
			latest = ""
		}
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

// CreateCommit creates a commit for the selected files on the current branch.
func (vm *VersionManager) CreateCommit(selectedFiles []string, message string) Commit {
	vm.Lock()
	defer vm.Unlock()
	dmp := diffmatchpatch.New()
	commit := Commit{
		ID:        vm.nextCommitID,
		Timestamp: time.Now(),
		Message:   message,
		Branch:    vm.currentBranch,
		Files:     make(map[string]FileVersion),
	}
	for _, file := range selectedFiles {
		if versions, exists := vm.fileVersions[file]; exists && len(versions) > 0 {
			baseline := ""
			if c, ok := vm.committedFiles[file]; ok {
				baseline = strings.TrimSpace(c)
			}
			currentVersion := versions[len(versions)-1]
			current := strings.TrimSpace(currentVersion.Content)
			diffs := dmp.DiffMain(baseline, current, false)
			dmp.DiffCleanupSemantic(diffs)
			diffText := formatDiff(diffs)
			fv := FileVersion{
				Timestamp: time.Now(),
				Content:   current,
				Diff:      diffText,
				Deleted:   currentVersion.Deleted,
			}
			commit.Files[file] = fv
			vm.committedFiles[file] = current
			vm.fileVersions[file] = []FileVersion{{Timestamp: time.Now(), Content: current, Deleted: currentVersion.Deleted}}
		}
	}
	vm.commits = append(vm.commits, commit)
	vm.nextCommitID++
	entry := fmt.Sprintf("Commit %d created on branch '%s'", commit.ID, vm.currentBranch)
	vm.auditLog = append(vm.auditLog, entry)
	_ = vm.storage.AppendAudit(entry)
	// Persist committedFiles.
	_ = vm.storage.SaveCommittedFiles(vm.committedFiles)
	// Save commit.
	_ = vm.storage.SaveEntity(commitsBucket, commit.ID, commit)
	log.Printf("Created commit %d on branch '%s': %s", commit.ID, vm.currentBranch, message)
	return commit
}

// MergeCommits merges all pending commits (for the current branch) into a version group.
func (vm *VersionManager) MergeCommits(tag string) (VersionGroup, error) {
	vm.Lock()
	defer vm.Unlock()
	var mergedMsg []string
	fileCommits := make(map[string][]string) // candidates for each file
	var conflicts []string
	for _, commit := range vm.commits {
		if commit.Branch != vm.currentBranch {
			continue
		}
		mergedMsg = append(mergedMsg, fmt.Sprintf("Commit %d: %s", commit.ID, commit.Message))
		for file, version := range commit.Files {
			candidate := strings.TrimSpace(version.Content)
			fileCommits[file] = append(fileCommits[file], candidate)
		}
	}
	mergedFiles := make(map[string]FileVersion)
	dmp := diffmatchpatch.New()
	for file, candidates := range fileCommits {
		base := ""
		if b, ok := vm.committedFiles[file]; ok {
			base = strings.TrimSpace(b)
		}
		merged, conflict := mergeFileVersions(base, candidates)
		if conflict {
			conflicts = append(conflicts, file)
			continue
		}
		finalDiff := formatDiff(dmp.DiffMain(base, merged, false))
		mergedFiles[file] = FileVersion{
			Timestamp: time.Now(),
			Content:   merged,
			Diff:      finalDiff,
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
		Branch:        vm.currentBranch,
		Files:         mergedFiles,
	}
	vm.versions = append(vm.versions, mergedVersion)
	vm.nextVerID++
	// Remove merged commits for current branch.
	var remainingCommits []Commit
	for _, commit := range vm.commits {
		if commit.Branch != vm.currentBranch {
			remainingCommits = append(remainingCommits, commit)
		}
	}
	vm.commits = remainingCommits
	entry := fmt.Sprintf("Merged commits on branch '%s' into version %d", vm.currentBranch, mergedVersion.ID)
	vm.auditLog = append(vm.auditLog, entry)
	_ = vm.storage.AppendAudit(entry)
	// Persist the merged version.
	_ = vm.storage.SaveEntity(versionsBucket, mergedVersion.ID, mergedVersion)
	log.Printf("Created version %d on branch '%s' with tag '%s'", mergedVersion.ID, vm.currentBranch, tag)
	return mergedVersion, nil
}

// MergeSelectedCommits merges only the commits identified by commitIDs.
func (vm *VersionManager) MergeSelectedCommits(commitIDs []int, tag string) (VersionGroup, error) {
	vm.Lock()
	defer vm.Unlock()
	var mergedMsg []string
	fileCommits := make(map[string][]string)
	var conflicts []string
	selectedMap := make(map[int]bool)
	for _, id := range commitIDs {
		selectedMap[id] = true
	}
	var remainingCommits []Commit
	for _, commit := range vm.commits {
		if commit.Branch != vm.currentBranch {
			remainingCommits = append(remainingCommits, commit)
			continue
		}
		if selectedMap[commit.ID] {
			mergedMsg = append(mergedMsg, fmt.Sprintf("Commit %d: %s", commit.ID, commit.Message))
			for file, fv := range commit.Files {
				fileCommits[file] = append(fileCommits[file], strings.TrimSpace(fv.Content))
			}
		} else {
			remainingCommits = append(remainingCommits, commit)
		}
	}
	mergedFiles := make(map[string]FileVersion)
	dmp := diffmatchpatch.New()
	for file, candidates := range fileCommits {
		base := ""
		if b, ok := vm.committedFiles[file]; ok {
			base = strings.TrimSpace(b)
		}
		merged, conflict := mergeFileVersions(base, candidates)
		if conflict {
			conflicts = append(conflicts, file)
			continue
		}
		finalDiff := formatDiff(dmp.DiffMain(base, merged, false))
		mergedFiles[file] = FileVersion{
			Timestamp: time.Now(),
			Content:   merged,
			Diff:      finalDiff,
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
		Branch:        vm.currentBranch,
		Files:         mergedFiles,
	}
	vm.versions = append(vm.versions, mergedVersion)
	vm.nextVerID++
	vm.commits = remainingCommits
	entry := fmt.Sprintf("Merged selected commits on branch '%s' into version %d", vm.currentBranch, mergedVersion.ID)
	vm.auditLog = append(vm.auditLog, entry)
	_ = vm.storage.AppendAudit(entry)
	_ = vm.storage.SaveEntity(versionsBucket, mergedVersion.ID, mergedVersion)
	log.Printf("Created version %d on branch '%s' with tag '%s' merging commits: %v", mergedVersion.ID, vm.currentBranch, tag, commitIDs)
	return mergedVersion, nil
}

// RevertPendingCommits discards pending commits on the current branch.
func (vm *VersionManager) RevertPendingCommits() {
	vm.Lock()
	defer vm.Unlock()
	var remaining []Commit
	for _, commit := range vm.commits {
		if commit.Branch != vm.currentBranch {
			remaining = append(remaining, commit)
		}
	}
	vm.commits = remaining
	entry := fmt.Sprintf("Pending commits on branch '%s' reverted.", vm.currentBranch)
	vm.auditLog = append(vm.auditLog, entry)
	_ = vm.storage.AppendAudit(entry)
	log.Println("Pending commits reverted.")
}

// AbortMerge logs that a merge was aborted.
func (vm *VersionManager) AbortMerge() {
	vm.Lock()
	defer vm.Unlock()
	entry := fmt.Sprintf("Merge aborted on branch '%s'", vm.currentBranch)
	vm.auditLog = append(vm.auditLog, entry)
	_ = vm.storage.AppendAudit(entry)
	log.Println("Merge aborted. No changes applied.")
}

// GetDiff returns a diff between the stored file and new content.
func (vm *VersionManager) GetDiff(filePath, newContent string) string {
	vm.RLock()
	defer vm.RUnlock()
	baseline := ""
	if content, ok := vm.latestFiles[filePath]; ok {
		baseline = content
	}
	dmp := diffmatchpatch.New()
	diffs := dmp.DiffMain(baseline, newContent, false)
	dmp.DiffCleanupSemantic(diffs)
	return formatDiff(diffs)
}

// SwitchBranch changes the current branch and persists it.
func (vm *VersionManager) SwitchBranch(branch string) {
	vm.Lock()
	defer vm.Unlock()
	vm.currentBranch = branch
	entry := fmt.Sprintf("Switched to branch '%s'", branch)
	vm.auditLog = append(vm.auditLog, entry)
	_ = vm.storage.AppendAudit(entry)
	_ = vm.storage.SaveBranch(branch)
	log.Printf("Switched to branch: %s", branch)
}

// RollbackDeployment reverts production to an earlier version and resets the baseline.
func (vm *VersionManager) RollbackDeployment(versionID int) error {
	vm.Lock()
	defer vm.Unlock()
	var target *VersionGroup
	for _, ver := range vm.versions {
		if ver.ID == versionID && ver.Branch == vm.currentBranch {
			target = &ver
			break
		}
	}
	if target == nil {
		return errors.New("version not found for rollback on current branch")
	}
	if err := deployVersion(*target); err != nil {
		return err
	}
	// Reset baseline committed files to target version.
	for file, fv := range target.Files {
		vm.committedFiles[file] = fv.Content
		vm.fileVersions[file] = []FileVersion{{Timestamp: time.Now(), Content: fv.Content, Deleted: fv.Deleted}}
	}
	vm.commits = []Commit{}
	vm.deployedVersion = target
	entry := fmt.Sprintf("Rolled back deployment to version %d on branch '%s'", target.ID, vm.currentBranch)
	vm.auditLog = append(vm.auditLog, entry)
	_ = vm.storage.AppendAudit(entry)
	_ = vm.storage.SaveCommittedFiles(vm.committedFiles)
	log.Printf("Rolled back deployment to version %d", target.ID)
	return nil
}

// --- File Watcher ---

func watchFiles(paths []string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	defer watcher.Close()
	for _, root := range paths {
		err = filepath.Walk(root, func(path string, info fs.FileInfo, err error) error {
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
			if event.Op&fsnotify.Remove != 0 || event.Op&fsnotify.Rename != 0 {
				versionManager.UpdateFile(event.Name, "", true)
				log.Printf("File removed: %s", event.Name)
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				data, err := ioutil.ReadFile(event.Name)
				if err != nil {
					log.Printf("Error reading %s: %v", event.Name, err)
					continue
				}
				content := string(data)
				diff := versionManager.GetDiff(event.Name, content)
				log.Printf("Change on %s\nDiff:\n%s", event.Name, diff)
				versionManager.UpdateFile(event.Name, content, false)
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Println("Watcher error:", err)
		}
	}
}

// --- HTTP Handlers and Basic Auth Middleware ---

func basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != Username || p != Password {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restricted"`)
			http.Error(w, "Unauthorized.", http.StatusUnauthorized)
			return
		}
		next(w, r)
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
	versionManager.RLock()
	defer versionManager.RUnlock()
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
	versionManager.RLock()
	defer versionManager.RUnlock()
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
		if ver.ID == payload.VersionID && ver.Branch == versionManager.currentBranch {
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
	// For switching version, do not reset baseline.
	versionManager.deployedVersion = selected
	entry := fmt.Sprintf("Switched to deployed version %d on branch '%s'", selected.ID, versionManager.currentBranch)
	versionManager.auditLog = append(versionManager.auditLog, entry)
	_ = versionManager.storage.AppendAudit(entry)
	log.Printf("Switched to deployed version %d", selected.ID)
	_ = json.NewEncoder(w).Encode(selected)
}

func handleDeployedVersion(w http.ResponseWriter, r *http.Request) {
	versionManager.RLock()
	defer versionManager.RUnlock()
	if versionManager.deployedVersion == nil {
		http.Error(w, "No deployed version", http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(versionManager.deployedVersion)
}

type SwitchBranchPayload struct {
	Branch string `json:"branch"`
}

func handleSwitchBranch(w http.ResponseWriter, r *http.Request) {
	var payload SwitchBranchPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || strings.TrimSpace(payload.Branch) == "" {
		http.Error(w, "Invalid branch payload", http.StatusBadRequest)
		return
	}
	versionManager.SwitchBranch(payload.Branch)
	_, _ = w.Write([]byte(fmt.Sprintf("Switched to branch '%s'", payload.Branch)))
}

type RollbackPayload struct {
	VersionID int `json:"version_id"`
}

func handleRollback(w http.ResponseWriter, r *http.Request) {
	var payload RollbackPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}
	if err := versionManager.RollbackDeployment(payload.VersionID); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	_, _ = w.Write([]byte(fmt.Sprintf("Rolled back deployment to version %d", payload.VersionID)))
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "./static/index.html")
}

func main() {
	// Initialize storage.
	st, err := NewStorage(dbFile)
	if err != nil {
		log.Fatalf("Error opening storage: %v", err)
	}
	defer st.db.Close()
	// Initialize VersionManager.
	versionManager = NewVersionManager(st)
	// Start file watcher.
	go watchFiles(watchPaths)
	// HTTP routes.
	http.HandleFunc("/api/changes", basicAuth(handleChanges))
	http.HandleFunc("/api/commit", basicAuth(handleCommit))
	http.HandleFunc("/api/commits", basicAuth(handleGetCommits))
	http.HandleFunc("/api/version", basicAuth(handleCreateVersion))
	http.HandleFunc("/api/version/mergeSelected", basicAuth(handleMergeSelectedCommits))
	http.HandleFunc("/api/version/revert", basicAuth(handleRevertCommits))
	http.HandleFunc("/api/merge/abort", basicAuth(handleAbortMerge))
	http.HandleFunc("/api/versions", basicAuth(handleGetVersions))
	http.HandleFunc("/api/version/switch", basicAuth(handleSwitchVersion))
	http.HandleFunc("/api/deployedVersion", basicAuth(handleDeployedVersion))
	http.HandleFunc("/api/branch/switch", basicAuth(handleSwitchBranch))
	http.HandleFunc("/api/deployment/rollback", basicAuth(handleRollback))
	http.HandleFunc("/", handleIndex)
	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))
	addr := ":8080"
	log.Printf("Server starting on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal(err)
	}
}
