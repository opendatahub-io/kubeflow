# E2E Structured Logging

This document describes the structured logging framework implemented for the E2E tests in the ODH Notebook Controller using the same logr infrastructure as the main controller.

## Overview

The E2E logging framework provides configurable, structured logging to help with debugging test failures, especially in CI environments. It uses the same logging infrastructure as the main controller (logr + zap) for consistency and professional-grade logging capabilities.

## Features

- **Multiple log levels**: `debug`, `info`, `warn`, `error`
- **Configurable via environment variable**: `E2E_LOG_LEVEL=debug|info|warn|error`
- **INFO level by default**: Provides basic information without overwhelming output
- **DEBUG level in CI**: Detailed logging enabled by default in CI environments
- **Structured logging**: Key-value pairs for easy filtering and analysis
- **Context-aware**: Different loggers for different test areas (helper, creation, culling, etc.)
- **Consistent with controller**: Uses the same logr/zap infrastructure

## Usage

### Log Levels

- **ERROR**: Only errors and failures (minimal output)
- **WARN**: Warnings and errors
- **INFO**: General information about test progress (default)
- **DEBUG**: Detailed debugging information including polling, status checks, etc.

### Local Development

```bash
# Default INFO level - clean, informative output
make e2e-test

# Debug level for troubleshooting
export E2E_LOG_LEVEL=debug
make e2e-test

# Minimal output - only errors
export E2E_LOG_LEVEL=error
make e2e-test
```

### CI Environment

In CI environments, debug logging is enabled by default but can be overridden:

```bash
# Default in CI - debug level
./run-e2e-test.sh

# Override to info level in CI
export E2E_LOG_LEVEL=info
./run-e2e-test.sh
```

## Log Format

Structured logs follow the standard logr/zap format with key-value pairs:

```
2023-09-15T15:04:05.123Z	INFO	e2e-tests.helper	Waiting for controller deployment	{"deployment": "odh-notebook-controller-manager", "replicas": 1, "timeout": "1m0s"}
2023-09-15T15:04:05.456Z	INFO	e2e-tests.creation	Starting notebook creation test	{"notebook": "thoth-minimal-oauth-notebook", "namespace": "e2e-notebook-controller"}
```

With structured data for easy parsing:

```
2023-09-15T15:04:10.789Z	INFO	e2e-tests.helper	Controller deployment ready	{"deployment": "odh-notebook-controller-manager", "duration": "5.333s", "polls": 3}
```

Debug level logs (V(1)) provide additional detail:

```
2023-09-15T15:04:06.123Z	DEBUG	e2e-tests.helper	Deployment status	{"poll": 2, "readyReplicas": 1, "expectedReplicas": 1, "availableReplicas": 1}
```

## Logger Contexts

The logging framework provides different logger contexts for different test areas:

- **helper** - Helper functions (waiting for deployments, getting routes, etc.)
- **creation** - Notebook creation tests
- **deletion** - Notebook deletion tests  
- **update** - Notebook update tests
- **culling** - Notebook culling tests
- **validation** - Validation tests

## Implementation Details

### Core Components

1. **debug_logger.go**: Structured logging framework using logr/zap
2. **Environment variable**: `E2E_LOG_LEVEL` controls log level (debug|info|warn|error)
3. **Context-based loggers**: Specialized loggers for different test areas
4. **Structured fields**: Key-value pairs for easy parsing and filtering

### Key Functions

- `GetLogger(name)` - Get a logger with custom name
- `GetHelperLogger()` - Helper function logging
- `GetCreationLogger()` - Creation test logging  
- `GetCullingLogger()` - Culling test logging
- `GetValidationLogger()` - Validation test logging

### Integration Points

The structured logging is integrated into:

- **Controller deployment waiting** - Polling status with structured data
- **Route retrieval** - Network policy and route discovery with context
- **Notebook creation** - Step-by-step creation process tracking
- **Culling tests** - Comprehensive workflow with timing and status
- **HTTP requests** - Request details and response status tracking

## Examples

### Sample Output (INFO Level - Default)

```
2023-09-15T15:04:05.123Z	INFO	e2e-tests	E2E logging initialized	{"level": "info"}
2023-09-15T15:04:05.200Z	INFO	e2e-tests.helper	Waiting for controller deployment	{"deployment": "odh-notebook-controller-manager", "replicas": 1, "timeout": "1m0s"}
2023-09-15T15:04:10.456Z	INFO	e2e-tests.helper	Controller deployment ready	{"duration": "5.256s", "polls": 3}
2023-09-15T15:04:11.123Z	INFO	e2e-tests.creation	Starting notebook creation test	{"notebook": "thoth-minimal-oauth-notebook", "namespace": "e2e-notebook-controller"}
2023-09-15T15:04:15.789Z	INFO	e2e-tests.creation	Notebook creation test completed successfully	{"duration": "4.666s"}
```

### Sample Output (DEBUG Level)

```
2023-09-15T15:04:05.123Z	INFO	e2e-tests	E2E logging initialized	{"level": "debug"}
2023-09-15T15:04:05.200Z	INFO	e2e-tests.helper	Waiting for controller deployment	{"deployment": "odh-notebook-controller-manager", "replicas": 1, "timeout": "1m0s"}
2023-09-15T15:04:06.250Z	DEBUG	e2e-tests.helper	Deployment not found, continuing to wait	{"poll": 1}
2023-09-15T15:04:08.300Z	DEBUG	e2e-tests.helper	Deployment status	{"poll": 2, "readyReplicas": 0, "expectedReplicas": 1, "availableReplicas": 0, "unavailableReplicas": 1}
2023-09-15T15:04:10.456Z	INFO	e2e-tests.helper	Controller deployment ready	{"duration": "5.256s", "polls": 3}
2023-09-15T15:04:11.123Z	INFO	e2e-tests.creation	Starting notebook creation test	{"notebook": "thoth-minimal-oauth-notebook", "namespace": "e2e-notebook-controller"}
2023-09-15T15:04:11.200Z	DEBUG	e2e-tests.creation	Checking if notebook already exists
2023-09-15T15:04:11.250Z	INFO	e2e-tests.creation	Notebook not found, creating new notebook
2023-09-15T15:04:15.789Z	INFO	e2e-tests.creation	Notebook creation test completed successfully	{"duration": "4.666s"}
```

### Sample Output (ERROR Level - Minimal)

```
2023-09-15T15:04:05.123Z	INFO	e2e-tests	E2E logging initialized	{"level": "error"}
# Only errors would be shown, providing minimal output for fast feedback
```

## Filtering and Analysis

### Filtering Logs

You can filter structured logs by context or fields using standard tools:

```bash
# Show only helper operations
make e2e-test 2>&1 | grep "e2e-tests.helper"

# Show only creation tests
make e2e-test 2>&1 | grep "e2e-tests.creation"

# Show only errors
make e2e-test 2>&1 | grep "ERROR"

# Filter by specific notebook
make e2e-test 2>&1 | grep '"notebook":"thoth-minimal-oauth-notebook"'
```

### JSON Processing

Since logs are structured, you can process them with tools like `jq`:

```bash
# Extract timing information
make e2e-test 2>&1 | grep "duration" | jq -r .duration

# Find operations that took longer than 5 seconds
make e2e-test 2>&1 | jq -r 'select(.duration > "5s")'
```

## Troubleshooting

### Common Issues

1. **Environment variable not set**: Ensure `E2E_LOG_LEVEL` is exported before running tests
2. **Invalid log level**: Valid values are `debug`, `info`, `warn`, `error` (case-insensitive)
3. **Override in CI**: To change CI logging level, set `E2E_LOG_LEVEL` before running scripts

### Performance Impact

- **ERROR level**: Minimal overhead, fastest execution
- **INFO level**: Low overhead, good balance (default)
- **DEBUG level**: Higher overhead but detailed information
- **Development mode**: DEBUG level enables development mode in zap for more readable output

## Contributing

When adding new E2E tests or modifying existing ones:

1. Use appropriate logger context (`GetCreationLogger()`, `GetHelperLogger()`, etc.)
2. Include structured fields for important data (`"notebook": name`, `"duration": time`)
3. Use `logger.Info()` for important events, `logger.V(1).Info()` for debug details
4. Use `logger.Error()` for error conditions with structured context
5. Add timing information for long-running operations

### Adding New Logger Contexts

To add a new logger context:

1. Add a new convenience function to `debug_logger.go`:
   ```go
   func GetMyNewLogger() logr.Logger {
       return e2eLogger.WithName("mynew")
   }
   ```
2. Use it in your tests:
   ```go
   logger := GetMyNewLogger().WithValues("key", "value")
   logger.Info("My operation started")
   ```
3. Update this documentation with the new context
