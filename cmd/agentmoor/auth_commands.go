package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"agentmoor/internal/platform"
	"golang.org/x/sys/unix"
)

// Credential contents are accepted only through a file, never a command-line
// bearer argument. Errors and success output never include secret material.
func authInput(path string, limit int64) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, platform.ErrAuthPolicyUnavailable
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, platform.ErrAuthPolicyUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, platform.ErrAuthPolicyUnavailable
	}
	return raw, nil
}

func authCommand(ctx context.Context, args []string, out io.Writer) error {
	flags := commandFlags(args[0])
	policy := flags.String("policy", "", "private authentication policy file")
	var keyID, fromKey, tokenFile *string
	switch args[0] {
	case "auth-init":
		keyID = flags.String("key-id", "", "initial key ID (generated when omitted)")
		fromKey = flags.String("from-key", "", "explicit legacy signing key to import")
	case "auth-rotate":
		keyID = flags.String("key-id", "", "new active key ID (generated when omitted)")
	case "auth-retire":
		keyID = flags.String("key-id", "", "verification key to retire")
	case "auth-revoke":
		tokenFile = flags.String("token-file", "", "private bearer credential file to revoke")
	default:
		return fmt.Errorf("unknown authentication command")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *policy == "" || flags.NArg() != 0 {
		return fmt.Errorf("%s requires --policy FILE", args[0])
	}
	now := time.Now()
	switch args[0] {
	case "auth-init":
		var legacy []byte
		if *fromKey != "" {
			var err error
			legacy, err = authInput(*fromKey, 32)
			if err != nil || len(legacy) != 32 {
				return platform.ErrAuthPolicyUnavailable
			}
		}
		id, err := platform.InitAuthPolicy(ctx, *policy, *keyID, legacy)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "authentication policy initialized; active key", id)
		return err
	case "auth-rotate":
		id, err := platform.RotateAuthPolicy(ctx, *policy, *keyID, now)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "authentication policy rotated; active key", id)
		return err
	case "auth-retire":
		if *keyID == "" {
			return fmt.Errorf("auth-retire requires --key-id ID")
		}
		if err := platform.RetireAuthPolicyKey(ctx, *policy, *keyID, now); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, "authentication verification key retired")
		return err
	case "auth-revoke":
		if *tokenFile == "" {
			return fmt.Errorf("auth-revoke requires --token-file FILE")
		}
		raw, err := authInput(*tokenFile, 4098)
		if err != nil {
			return err
		}
		token := strings.TrimSpace(string(raw))
		if err := platform.RevokeAuthPolicyCredential(ctx, *policy, token, now); err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, "authentication credential revoked")
		return err
	}
	return fmt.Errorf("unknown authentication command")
}
