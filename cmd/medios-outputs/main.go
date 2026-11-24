// medios-outputs is the outputs package as a binary, for a finish script
// that is not Go.
//
//	medios-outputs publish <attempt-dir> <manifest.json>
//	    hashes the artifacts the manifest names and commits manifest.json
//	    into the attempt directory; the input file is the manifest without
//	    sha256, bytes or published_at. Exit 0 on commit, 2 on refusal.
//	medios-outputs verify <path/to/manifest.json>
//	    prints "complete", "missing <name>", "hash mismatch <name>",
//	    "size mismatch <name>" or "invalid: <why>", then one
//	    "undeclared <path>" line per file in the attempt directory that
//	    the manifest does not name. "complete" means every artifact the
//	    manifest names is intact; it says nothing about undeclared files.
//	    Exit 0 only on complete.
//	medios-outputs list <store-root>
//	    one line per attempt: namespace/job/attempt and committed or
//	    uncommitted; a directory whose name is not a valid component is
//	    printed as "malformed <path>: <why>".
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/medios-lab/medios-sdk-go/outputs"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "publish":
		if len(os.Args) != 4 {
			usage()
		}
		raw, err := os.ReadFile(os.Args[3])
		if err != nil {
			fail("read %s: %v", os.Args[3], err)
		}
		var m outputs.Manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			fail("%s does not parse: %v", os.Args[3], err)
		}
		if err := outputs.Publish(os.Args[2], &m); err != nil {
			fail("%v", err)
		}
		fmt.Printf("committed %s/%s/%s with %d artifacts\n", m.Namespace, m.Job, m.Attempt, len(m.Artifacts))
	case "verify":
		if len(os.Args) != 3 {
			usage()
		}
		res, err := outputs.Verify(os.Args[2])
		if err != nil {
			fail("%v", err)
		}
		fmt.Println(res.String())
		for _, u := range res.Undeclared {
			fmt.Printf("undeclared %s\n", u)
		}
		if !res.Complete() {
			os.Exit(1)
		}
	case "list":
		if len(os.Args) != 3 {
			usage()
		}
		entries, err := outputs.List(os.Args[2])
		if err != nil {
			fail("%v", err)
		}
		for _, e := range entries {
			if e.Malformed != "" {
				fmt.Printf("malformed %s: %s\n", e.Dir, e.Malformed)
				continue
			}
			state := "uncommitted"
			if e.Committed {
				state = "committed"
			}
			fmt.Printf("%s/%s/%s %s\n", e.Namespace, e.Job, e.Attempt, state)
		}
	default:
		usage()
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "medios-outputs: "+format+"\n", args...)
	os.Exit(2)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: medios-outputs publish <attempt-dir> <manifest.json> | verify <manifest.json> | list <store-root>")
	os.Exit(2)
}
