// release-manifest creates the offline Ed25519 key and signed update metadata.
// The private key must be kept outside this repository and injected into release CI.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/fivemanage/db-agent/internal/update"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: release-manifest keygen|public-key|sign [flags]")
	}

	switch args[0] {
	case "keygen":
		flags := flag.NewFlagSet("keygen", flag.ContinueOnError)
		privatePath := flags.String("private", "", "path for a new private key")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *privatePath == "" || flags.NArg() != 0 {
			return errors.New("keygen requires --private PATH")
		}

		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}

		file, err := os.OpenFile(*privatePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintln(file, base64.StdEncoding.EncodeToString(private)); err != nil {
			file.Close()
			os.Remove(*privatePath)
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}

		fmt.Println(base64.StdEncoding.EncodeToString(public))

		return nil
	case "public-key":
		flags := flag.NewFlagSet("public-key", flag.ContinueOnError)
		privatePath := flags.String("private", "", "private key file")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *privatePath == "" || flags.NArg() != 0 {
			return errors.New("public-key requires --private PATH")
		}

		private, err := readPrivateKey(*privatePath)
		if err != nil {
			return err
		}

		fmt.Println(base64.StdEncoding.EncodeToString(private.Public().(ed25519.PublicKey)))

		return nil
	case "sign":
		flags := flag.NewFlagSet("sign", flag.ContinueOnError)
		privatePath := flags.String("private", "", "private key file")
		artifact := flags.String("artifact", "", "release executable")
		version := flags.String("version", "", "stable major.minor.patch version")
		goos := flags.String("os", "", "target operating system")
		goarch := flags.String("arch", "", "target architecture")
		artifactURL := flags.String("url", "", "HTTPS artifact URL")
		output := flags.String("output", "", "new manifest JSON path")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *privatePath == "" || *artifact == "" || *version == "" || *goos == "" || *goarch == "" || *artifactURL == "" || *output == "" || flags.NArg() != 0 {
			return errors.New("sign requires --private, --artifact, --version, --os, --arch, --url, and --output")
		}

		private, err := readPrivateKey(*privatePath)
		if err != nil {
			return err
		}

		file, err := os.Open(*artifact)
		if err != nil {
			return err
		}
		defer file.Close()

		hash := sha256.New()
		if _, err := io.Copy(hash, file); err != nil {
			return err
		}

		manifest := update.Manifest{Version: *version, OS: *goos, Arch: *goarch, URL: *artifactURL, SHA256: hex.EncodeToString(hash.Sum(nil))}
		manifest.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(private, manifest.Payload()))
		if _, err := manifest.Verify(private.Public().(ed25519.PublicKey), "0.0.0", *goos, *goarch); err != nil {
			return err
		}

		outputFile, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(outputFile)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(manifest); err != nil {
			outputFile.Close()
			os.Remove(*output)
			return err
		}

		return outputFile.Close()
	default:
		return errors.New("usage: release-manifest keygen|public-key|sign [flags]")
	}
}

func readPrivateKey(path string) (ed25519.PrivateKey, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	private, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
	if err != nil || len(private) != ed25519.PrivateKeySize {
		return nil, errors.New("private key must be base64-encoded Ed25519")
	}

	return ed25519.PrivateKey(private), nil
}
