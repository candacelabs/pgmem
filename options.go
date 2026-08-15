package pgmem

import "fmt"

type config struct {
	translator Translator
}

// Option configures a database before its first connection is opened.
type Option func(*config) error

// WithTranslator replaces the PostgreSQL compatibility translator. This is
// primarily useful for adding project-specific syntax during tests.
func WithTranslator(translator Translator) Option {
	return func(cfg *config) error {
		if translator == nil {
			return fmt.Errorf("pgmem: translator must not be nil")
		}
		cfg.translator = translator
		return nil
	}
}
