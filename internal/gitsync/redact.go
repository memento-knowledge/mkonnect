package gitsync

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
)

// minRedactLength is the shortest secret that is redacted from git's output. A very short string would
// also match ordinary words and wreck the text; the origin token is validated to at least 16
// characters, and a platform push token this short would be unusable anyway.
const minRedactLength = 8

const redacted = "<redacted>"

// secretForms returns every form in which a secret could appear in text git prints: the token itself,
// and — for basic authentication, where git sends base64("user:token") — that encoding. A server that
// reflects the Authorization header it received returns one of them.
func secretForms(origin LocalEntry, pushToken Secret) [][]byte {
	var forms [][]byte
	add := func(s string) {
		if len(s) >= minRedactLength {
			forms = append(forms, []byte(s))
		}
	}
	add(origin.Token.Reveal())
	add(pushToken.Reveal())
	if origin.Auth == "basic" {
		add(base64.StdEncoding.EncodeToString([]byte(origin.Username + ":" + origin.Token.Reveal())))
	}
	return forms
}

// redactSecrets replaces every occurrence of a secret form in b.
func redactSecrets(b []byte, forms [][]byte) []byte {
	for _, f := range forms {
		b = bytes.ReplaceAll(b, f, []byte(redacted))
	}
	return b
}

// redactingStderr returns d with its git runners wrapped so the stderr they return has t's credentials
// removed before anything reads it. It is a backstop behind the classifier, which already withholds
// text the remote end sent: whatever else git prints, a credential this sync holds does not reach an
// error detail, a log line or a status.
func (d syncDeps) redactingStderr(t syncTask) syncDeps {
	forms := secretForms(t.Origin, t.PushToken)
	if len(forms) == 0 {
		return d
	}
	if run := d.run; run != nil {
		d.run = func(ctx context.Context, argv, env []string, dir string) ([]byte, error) {
			stderr, err := run(ctx, argv, env, dir)
			return redactSecrets(stderr, forms), err
		}
	}
	if runOut := d.runOut; runOut != nil {
		d.runOut = func(ctx context.Context, argv, env []string, dir string, stdout io.Writer) ([]byte, error) {
			stderr, err := runOut(ctx, argv, env, dir, stdout)
			return redactSecrets(stderr, forms), err
		}
	}
	return d
}
