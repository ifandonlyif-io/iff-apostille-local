// apostille-local-verify verifies exported metadata records without a server.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/evidence"
	core "github.com/ifandonlyif-io/iff-apostille/apostille"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("apostille-local-verify", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	artifactPath := flags.String("artifact", "", "local run-manifest.json")
	bundlePath := flags.String("bundle", "", "local run-bundle.json")
	producerPin := flags.String("producer-pin", "", "independently selected sha256: producer fingerprint")
	requirePQ := flags.Bool("require-post-quantum", false, "accept only Core 0.3 (ML-DSA-65) records")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *artifactPath == "" || *bundlePath == "" || *producerPin == "" {
		return errors.New("usage: apostille-local-verify --artifact FILE --bundle FILE --producer-pin sha256:FINGERPRINT [--require-post-quantum]")
	}
	artifact, err := readFile(*artifactPath, evidence.MaxRecordBytes)
	if err != nil {
		return err
	}
	bundle, err := readFile(*bundlePath, core.MaxInputBytes)
	if err != nil {
		return err
	}
	verified, err := evidence.VerifyWith(artifact, bundle, *producerPin, evidence.VerifyOptions{RequirePostQuantum: *requirePQ})
	if err != nil {
		_ = json.NewEncoder(output).Encode(map[string]any{"valid": false, "error": "verification_failed"})
		return errors.New("verification_failed")
	}
	return json.NewEncoder(output).Encode(struct {
		Valid bool `json:"valid"`
		evidence.Verification
	}{true, verified})
}

func readFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("unreadable_or_oversized_file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("unreadable_or_oversized_file")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("unreadable_or_oversized_file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("unreadable_or_oversized_file")
	}
	return raw, nil
}
