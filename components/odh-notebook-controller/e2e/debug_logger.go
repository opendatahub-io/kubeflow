package e2e

import (
	"os"
	"time"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
)

// e2eLogger is the structured logger for E2E tests
var e2eLogger logr.Logger

func init() {
	// Initialize structured logger using the same infrastructure as the controller
	logLevel := getLogLevel()

	opts := ctrlzap.Options{
		Development: logLevel <= zapcore.DebugLevel,
		Level:       logLevel,
		TimeEncoder: zapcore.TimeEncoderOfLayout(time.RFC3339),
	}

	zapLogger := ctrlzap.New(ctrlzap.UseFlagOptions(&opts))
	e2eLogger = zapLogger.WithName("e2e-tests")

	// Log initialization
	e2eLogger.Info("E2E logging initialized", "level", logLevel.String())
}

// getLogLevel determines log level from environment variable
// E2E_LOG_LEVEL can be: debug, info, warn, error
// Default is INFO level for normal operation
func getLogLevel() zapcore.Level {
	envValue := os.Getenv("E2E_LOG_LEVEL")
	switch envValue {
	case "debug", "DEBUG":
		return zapcore.DebugLevel
	case "info", "INFO":
		return zapcore.InfoLevel
	case "warn", "WARN":
		return zapcore.WarnLevel
	case "error", "ERROR":
		return zapcore.ErrorLevel
	default:
		// Default to INFO level (normal operation)
		// In CI, we can set E2E_LOG_LEVEL=debug for detailed logging
		return zapcore.InfoLevel
	}
}

// GetLogger returns a logger with the specified context name
func GetLogger(name string) logr.Logger {
	return e2eLogger.WithName(name)
}

// Convenience functions for common E2E test contexts
// These provide pre-configured loggers for different test areas

// GetHelperLogger returns a logger for helper functions
func GetHelperLogger() logr.Logger {
	return e2eLogger.WithName("helper")
}

// GetCreationLogger returns a logger for creation tests
func GetCreationLogger() logr.Logger {
	return e2eLogger.WithName("creation")
}

// GetDeletionLogger returns a logger for deletion tests
func GetDeletionLogger() logr.Logger {
	return e2eLogger.WithName("deletion")
}

// GetUpdateLogger returns a logger for update tests
func GetUpdateLogger() logr.Logger {
	return e2eLogger.WithName("update")
}

// GetCullingLogger returns a logger for culling tests
func GetCullingLogger() logr.Logger {
	return e2eLogger.WithName("culling")
}

// GetValidationLogger returns a logger for validation tests
func GetValidationLogger() logr.Logger {
	return e2eLogger.WithName("validation")
}

// IsDebugEnabled returns whether debug level logging is enabled
func IsDebugEnabled() bool {
	return e2eLogger.V(1).Enabled()
}
