package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Launcher identity binding modes, kept in step with the dnfbridge constants of
// the same names. They are duplicated here rather than imported because the
// controller is the config generator and must not depend on the protocol
// service just to spell three strings.
const (
	launcherBindingModeOff     = "off"
	launcherBindingModeAudit   = "audit"
	launcherBindingModeEnforce = "enforce"
)

// launcherIdentityPrivateKeyFile is where keygen writes the launcher's half.
// The name says "private" because the file is the whole identity: whoever holds
// it can connect to this server, and the server package must never ship it.
const launcherIdentityPrivateKeyFile = "launcher_identity_private.pem"

func normalizeLauncherBindingMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case launcherBindingModeAudit, "log":
		return launcherBindingModeAudit
	case launcherBindingModeEnforce, "on", "true", "required":
		return launcherBindingModeEnforce
	default:
		return launcherBindingModeOff
	}
}

func normalizeLauncherBindingKeys(keys []string) []string {
	normalized := make([]string, 0, len(keys))
	for _, key := range keys {
		if trimmed := strings.TrimSpace(key); trimmed != "" {
			normalized = append(normalized, trimmed)
		}
	}
	return normalized
}

// launcherIdentity generates the deployment key pair, stores the public half in
// instance.json, and writes the private half where the launcher build picks it
// up. Only the public half ever reaches the server, which is what stops a
// distributed server package from minting its own proof.
func (c *controller) launcherIdentity(_ context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("launcher-identity subcommand is required: keygen or status")
	}
	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "keygen":
		fs := newFlagSet("launcher-identity keygen", c.stderr)
		mode := fs.String("mode", launcherBindingModeAudit,
			"binding mode to write: off, audit or enforce")
		out := fs.String("out", "", "directory for the private key file (default runtime/config)")
		force := fs.Bool("force", false, "replace an existing key pair")
		if err := parseFlags(fs, args[1:]); err != nil {
			return err
		}
		return c.generateLauncherIdentity(*mode, *out, *force)
	case "status":
		fs := newFlagSet("launcher-identity status", c.stderr)
		if err := parseFlags(fs, args[1:]); err != nil {
			return err
		}
		return c.launcherIdentityStatus()
	default:
		return fmt.Errorf("unknown launcher-identity subcommand %q; use keygen or status", args[0])
	}
}

func (c *controller) generateLauncherIdentity(mode string, out string, force bool) error {
	normalized := normalizeLauncherBindingMode(mode)
	if normalized == launcherBindingModeOff && !strings.EqualFold(strings.TrimSpace(mode), launcherBindingModeOff) {
		return fmt.Errorf("mode must be one of %s, %s or %s", launcherBindingModeOff, launcherBindingModeAudit, launcherBindingModeEnforce)
	}

	// loadInstance applies the same migrations and AUTO_DETECT resolution the
	// server will see, so this writes back a config that is already valid rather
	// than re-validating a raw file that still says AUTO_DETECT.
	current, err := loadInstance(c.paths)
	if err != nil {
		return err
	}
	if current.LauncherBinding != nil && !force {
		if existing := normalizeLauncherBindingKeys(current.LauncherBinding.PublicKeys); len(existing) > 0 {
			return fmt.Errorf("instance.json already pins %d launcher public key(s); pass --force to rotate them", len(existing))
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate launcher identity key: %w", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return fmt.Errorf("encode launcher public key: %w", err)
	}
	publicEncoded := base64.StdEncoding.EncodeToString(publicDER)

	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return fmt.Errorf("encode launcher private key: %w", err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})

	targetDir := strings.TrimSpace(out)
	if targetDir == "" {
		targetDir = c.paths.runtimeConfig
	}
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		return fmt.Errorf("create private key directory: %w", err)
	}
	privatePath := filepath.Join(targetDir, launcherIdentityPrivateKeyFile)
	if err := writeFile(privatePath, privatePEM, 0o600); err != nil {
		return err
	}

	allowWireGuard := current.LauncherBinding != nil && current.LauncherBinding.AllowWireGuard
	// The key is kept even in off mode: parking the binding must not throw away
	// the identity the distributed launcher already carries.
	current.LauncherBinding = &launcherBindingConfig{
		Mode:           normalized,
		PublicKeys:     []string{publicEncoded},
		AllowWireGuard: allowWireGuard,
	}
	if err := validateInstance(current); err != nil {
		return err
	}
	data, err := marshalInstance(current)
	if err != nil {
		return err
	}
	if err := writeFile(c.paths.instance, data, 0o600); err != nil {
		return err
	}

	fmt.Fprintf(c.stdout, "Launcher identity created.\n")
	fmt.Fprintf(c.stdout, "  binding mode : %s\n", normalized)
	fmt.Fprintf(c.stdout, "  public key   : %s\n", publicEncoded)
	fmt.Fprintf(c.stdout, "  private key  : %s\n", privatePath)
	fmt.Fprintf(c.stdout, "  instance     : %s\n\n", c.paths.instance)
	fmt.Fprint(c.stdout, launcherIdentityInstructions(normalized, publicEncoded))
	return nil
}

// launcherIdentityInstructions is the operator-facing half of keygen. It spells
// out where the private key has to end up, because a key that is generated but
// not installed looks exactly like a working setup until the first connection is
// refused.
func launcherIdentityInstructions(mode string, publicEncoded string) string {
	_ = publicEncoded
	var builder strings.Builder
	builder.WriteString("Next steps:\n")
	builder.WriteString("  1. Put the private key where the launcher reads it: copy it next to the launcher\n")
	builder.WriteString("     exe (or into %APPDATA%\\DNFLogin) as launcher_identity.key, or paste its\n")
	builder.WriteString("     base64 into EmbeddedPrivateKeyBase64 in Services\\LauncherIdentity.cs.\n")
	builder.WriteString("  2. Rebuild 90CN.dll from client-patch so the in-band LANA frame carries the lease.\n")
	builder.WriteString("  3. Rebuild and redistribute the launcher, then restart the server.\n")
	if mode == launcherBindingModeEnforce {
		builder.WriteString("\nMode is enforce: every game connection now needs a lease signed by this key.\n")
		builder.WriteString("Run with --mode audit first if the launcher or the client patch is not deployed yet.\n")
	}
	return builder.String()
}

func (c *controller) launcherIdentityStatus() error {
	current, err := loadInstance(c.paths)
	if err != nil {
		return err
	}
	binding := current.LauncherBinding
	mode := launcherBindingModeOff
	keys := []string(nil)
	allowWireGuard := false
	if binding != nil {
		mode = normalizeLauncherBindingMode(binding.Mode)
		keys = normalizeLauncherBindingKeys(binding.PublicKeys)
		allowWireGuard = binding.AllowWireGuard
	}
	fmt.Fprintf(c.stdout, "launcher binding mode : %s\n", mode)
	fmt.Fprintf(c.stdout, "pinned public keys    : %d\n", len(keys))
	for index, key := range keys {
		fmt.Fprintf(c.stdout, "  [%d] %s\n", index, key)
	}
	fmt.Fprintf(c.stdout, "allow WireGuard bind  : %t\n", allowWireGuard)
	privatePath := filepath.Join(c.paths.runtimeConfig, launcherIdentityPrivateKeyFile)
	if isRegularFile(privatePath) {
		fmt.Fprintf(c.stdout, "private key on disk   : %s\n", privatePath)
	} else {
		fmt.Fprintf(c.stdout, "private key on disk   : (none at %s)\n", privatePath)
	}
	if mode != launcherBindingModeOff && len(keys) == 0 {
		fmt.Fprintln(c.stderr, "WARNING: a mode is set but no public key is pinned; the server will refuse to start.")
	}
	return nil
}
