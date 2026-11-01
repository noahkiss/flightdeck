/// Fixed coding phrases for the accuracy test. Each is written the way it is spoken,
/// so word error rate compares like with like after normalisation
/// ("link_test.go" and "link underscore test dot go" both normalise to the same words).
enum Phrases {
    static let all: [String] = [
        "run make race with count three",
        "open internal slash link slash link underscore test dot go",
        "git commit dash dash amend then push dash dash force",
        "run go test dash run test goldens dash update",
        "rename handle input to handle pane input",
        "check the holder proto package for a data race",
        "open readme dot md",
        "add a nil check before calling json unmarshal",
        "set max retries to five",
        "use suspense query in the dashboard component",
        "run swift test in swift slash hab api",
        "revert the last commit and push to origin main",
        "grep for todo in the cmd directory",
        "open package dot swift and add fluid audio",
        "the function returns an error when the context is cancelled",
        "run make check gen then make vet",
        "export log underscore level equals debug",
        "stage only the changes in internal slash control",
        "add a json tag called pane id to the struct",
        "deny the approval and stop the session",
    ]
}
