package notify

import "context"

// SendText sends one message on a configured channel without queueing it (used by scheduled reports, which keep their
// own once-a-month claim and retry). build receives the public centre URL so the text can link to it.
func (s *Store) SendText(ctx context.Context, channel string, build func(centerURL string) string) error {
	view, ch, secret, err := s.configuration(ctx, channel)
	if err != nil {
		return err
	}
	return s.Sender.Send(ctx, channel, ch.To, secret, build(view.CenterURL))
}
