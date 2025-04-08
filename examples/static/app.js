document.addEventListener("DOMContentLoaded", function () {
    const devModeBtn = document.getElementById("devModeBtn");
    const prodModeBtn = document.getElementById("prodModeBtn");
    const devSection = document.getElementById("devSection");
    const prodSection = document.getElementById("prodSection");
    const commitBtn = document.getElementById("commitBtn");
    const mergeSelectedBtn = document.getElementById("mergeSelectedBtn");
    const switchVersionBtn = document.getElementById("switchVersionBtn");

    // Switch to Developer Mode.
    devModeBtn.addEventListener("click", () => {
        devSection.classList.remove("hidden");
        prodSection.classList.add("hidden");
        loadFileChanges();
        loadCommits();
    });

    // Switch to Production Mode.
    prodModeBtn.addEventListener("click", () => {
        prodSection.classList.remove("hidden");
        devSection.classList.add("hidden");
        loadVersions();
        loadDeployedVersion();
    });

    // Load file changes for commit creation.
    function loadFileChanges() {
        const fileChangesDiv = document.getElementById("fileChanges");
        const fileSelectList = document.getElementById("fileSelectList");
        fetch("/api/changes")
            .then(response => response.json())
            .then(data => {
                let changesHTML = "";
                let fileListHTML = "";
                for (let file in data) {
                    // Only include files with non-empty diff.
                    if (!data[file].trim()) continue;
                    const diffLines = data[file].split("\n").map(line => {
                        if (line.startsWith("+"))
                            return `<span class="text-green-600">${line}</span>`;
                        else if (line.startsWith("-"))
                            return `<span class="text-red-600">${line}</span>`;
                        return `<span>${line}</span>`;
                    }).join("<br>");
                    changesHTML += `<div class="mb-4 border-b pb-2">
                            <h3 class="font-semibold">${file}</h3>
                            <pre class="bg-gray-50 p-2 rounded text-sm overflow-x-auto">${diffLines}</pre>
                          </div>`;
                    fileListHTML += `<div>
                              <label>
                                <input type="checkbox" class="commitFile" value="${file}"> ${file}
                              </label>
                            </div>`;
                }
                fileChangesDiv.innerHTML = changesHTML || "<p>No changes detected.</p>";
                fileSelectList.innerHTML = fileListHTML || "<p>No files available for commit.</p>";
            })
            .catch(err => {
                fileChangesDiv.innerHTML = "<p>Error loading changes.</p>";
                console.error(err);
            });
    }

    // Load pending commits as accordions.
    function loadCommits() {
        fetch("/api/commits")
            .then(response => response.json())
            .then(commits => {
                let html = "";
                commits.forEach(commit => {
                    let filesHTML = "";
                    for (let file in commit.files) {
                        filesHTML += `<details class="ml-4 mb-2 border rounded">
                                <summary>File: ${file}</summary>
                                <div class="p-2">
                                    <p><strong>Content:</strong></p>
                                    <pre class="bg-gray-50 p-2 rounded text-sm overflow-x-auto">${commit.files[file].content}</pre>
                                    <p><strong>Diff:</strong></p>
                                    <pre class="bg-gray-50 p-2 rounded text-sm overflow-x-auto">${commit.files[file].diff || "No diff"}</pre>
                                </div>
                             </details>`;
                    }
                    html += `<details class="mb-4 border rounded">
                              <summary>
                                <input type="checkbox" class="mergeCommitCheckbox" value="${commit.id}">
                                Commit ${commit.id} - ${commit.message} <span class="text-sm text-gray-600">[${new Date(commit.timestamp).toLocaleString()}]</span>
                              </summary>
                              <div class="p-2">
                                ${filesHTML}
                              </div>
                           </details>`;
                });
                document.getElementById("commitsList").innerHTML = html || "<p>No pending commits.</p>";
            });
    }

    // Modified loadVersions: show versions as accordions with radio buttons.
    function loadVersions() {
        fetch("/api/versions")
            .then(response => response.json())
            .then(versions => {
                let html = "";
                versions.forEach((ver, index) => {
                    let filesHTML = "";
                    for (let file in ver.files) {
                        filesHTML += `<details class="ml-4 mb-2 border rounded">
                              <summary>File: ${file}</summary>
                              <div class="p-2">
                                  <p><strong>Content:</strong></p>
                                  <pre class="bg-gray-50 p-2 rounded text-sm overflow-x-auto">${ver.files[file].content}</pre>
                              </div>
                           </details>`;
                    }
                    html += `<div class="mb-4 border rounded p-2">
                              <label class="flex items-center">
                                <input type="radio" name="versionRadio" value="${ver.id}" ${index === 0 ? "checked" : ""} class="mr-2">
                                Version ${ver.id} ${ver.tag ? "- " + ver.tag : ""} <span class="text-sm text-gray-600">[${new Date(ver.timestamp).toLocaleString()}]</span>
                              </label>
                              <details class="mt-2 border rounded">
                                <summary>Show Version Details</summary>
                                <div class="p-2">
                                    <p><strong>Commit Message:</strong> ${ver.commitMessage}</p>
                                    ${filesHTML}
                                </div>
                              </details>
                           </div>`;
                });
                document.getElementById("versionsList").innerHTML = html || "<p>No versions created.</p>";
                // Removed auto-deploy. Instruct user to select a version.
                document.getElementById("deployedVersion").innerHTML = "<p>Please select a version to deploy.</p>";
            });
    }

    // Modified loadDeployedVersion to fetch the deployed version.
    function loadDeployedVersion() {
        fetch("/api/deployedVersion")
            .then(response => {
                if (!response.ok) {
                    document.getElementById("deployedVersion").innerHTML = "<p>No version deployed.</p>";
                    return null;
                }
                return response.json();
            })
            .then(ver => {
                if (!ver) return;
                let filesHTML = "";
                for (let file in ver.files) {
                    filesHTML += `<div class="mb-2 border rounded p-2">
                            <strong>File: ${file}</strong>
                            <pre class="bg-gray-50 p-2 rounded text-sm overflow-x-auto">${ver.files[file].content}</pre>
                         </div>`;
                }
                document.getElementById("deployedVersion").innerHTML = `<div>
                            <h3 class="text-xl font-bold">Deployed Version ${ver.id} ${ver.tag ? "- " + ver.tag : ""}</h3>
                            <p><strong>Commit Message: </strong>${ver.commitMessage}</p>
                            ${filesHTML}
                         </div>`;
            })
            .catch(err => console.error(err));
    }

    // Global function to respond to version switch.
    window.switchVersion = function (versionID) {
        console.log("Switched to version " + versionID);
        loadDeployedVersion();
    };

    // Create a commit.
    commitBtn.addEventListener("click", function () {
        const commitMsg = document.getElementById("commitMsgInput").value;
        const checkboxes = document.getElementsByClassName("commitFile");
        let selectedFiles = [];
        for (let cb of checkboxes) {
            if (cb.checked) {
                selectedFiles.push(cb.value);
            }
        }
        if (selectedFiles.length === 0) {
            alert("Please select at least one file to commit.");
            return;
        }
        const payload = {
            message: commitMsg,
            files: selectedFiles
        };
        fetch("/api/commit", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(payload)
        })
            .then(response => response.json())
            .then(() => {
                alert("Commit created.");
                loadCommits();
                loadFileChanges(); // refresh file changes so committed files don't appear
            })
            .catch(err => console.error(err));
    });

    // Merge Selected Commits.
    mergeSelectedBtn.addEventListener("click", function () {
        const tag = document.getElementById("mergeTagInput").value;
        const checkboxes = document.getElementsByClassName("mergeCommitCheckbox");
        let selectedIDs = [];
        for (let cb of checkboxes) {
            if (cb.checked) {
                selectedIDs.push(parseInt(cb.value));
            }
        }
        if (selectedIDs.length === 0) {
            alert("Please select at least one commit to merge.");
            return;
        }
        const payload = { tag: tag, commit_ids: selectedIDs };
        fetch("/api/version/mergeSelected", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(payload)
        })
            .then(response => {
                if (!response.ok) {
                    return response.text().then(error => { throw new Error(error); });
                }
                return response.json();
            })
            .then(data => {
                alert(`Version ${data.id} created.`);
                loadVersions();
                loadCommits();
                loadFileChanges();
            })
            .catch(err => {
                alert("Merge error: " + err.message);
            });
    });

    // Update Switch Version button event handler.
    switchVersionBtn.addEventListener("click", function () {
        const radios = document.getElementsByName("versionRadio");
        let selected = null;
        for (let r of radios) {
            if (r.checked) {
                selected = r.value;
                break;
            }
        }
        if (selected) {
            const payload = { version_id: parseInt(selected) };
            fetch("/api/version/switch", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify(payload)
            })
                .then(response => {
                    if (!response.ok) {
                        return response.text().then(error => { throw new Error(error); });
                    }
                    return response.json();
                })
                .then(data => {
                    alert(`Switched to version ${data.id}`);
                    loadDeployedVersion();
                })
                .catch(err => {
                    alert("Switch version error: " + err.message);
                });
        } else {
            alert("Please select a version.");
        }
    });

    // Start with Developer Mode.
    devModeBtn.click();
});
