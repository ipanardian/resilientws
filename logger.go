package resilientws

import "log"

type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Error(msg string, args ...any)
}

type defaultLogger struct{}

func (l *defaultLogger) Debug(msg string, args ...any) {
	log.Printf("DEBUG: "+msg, args...)
}

func (l *defaultLogger) Info(msg string, args ...any) {
	log.Printf("INFO: "+msg, args...)
}

func (l *defaultLogger) Error(msg string, args ...any) {
	log.Printf("ERROR: "+msg, args...)
}
