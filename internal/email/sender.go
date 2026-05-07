// Package email sends transactional mail. The Sender interface lets the
// rest of the app stay agnostic to the underlying provider — production
// uses Resend; dev falls back to LogSender so you can click magic links
// from container logs without burning quota or risking real sends.
package email

import "context"

type Sender interface {
	Send(ctx context.Context, msg Message) error
}

type Message struct {
	To       string
	Subject  string
	HTMLBody string
	TextBody string
}
