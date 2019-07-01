package logger

import (
	"bytes"
	"context"
	"io"
	"io/ioutil"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
	"go.elastic.co/apm"
	"go.elastic.co/apm/module/apmhttp"
	"google.golang.org/grpc/metadata"
)

const (
	redacted     = "[REDACTED]"
	cookieHeader = "Cookie"
)

var defaultSanitizedFieldNames = []string{
	`^password`,
	`^passwd`,
	`^pwd`,
	`^secret`,
	`^*key`,
	`^*token*`,
	`^*session*`,
	`^*credit*`,
	`^*card*`,
	`^authorization`,
	`^set-cookie`,
	`^phpsessididp`,
}

// Config is the configuration struct for the logger,
// Logger - a logrus Logger to use in the logger.
// SkipPath - path to skip logging.
// SkipPathRegexp - a regex to skip paths.
type Config struct {
	Logger             *logrus.Logger
	SkipBodyPath       []string
	SkipBodyPathRegexp *regexp.Regexp
	SkipPath           []string
	SkipPathRegexp     *regexp.Regexp
}

type request struct {
	Cookies []*http.Cookie
	Headers map[string][]string
	Form    map[string][]string
}

// SetLogger initializes the logging middleware.
func SetLogger(config *Config) gin.HandlerFunc {
	config = setupConfig(config)

	return func(c *gin.Context) {
		start := time.Now()
		fullPath := getRequestFullPath(c)
		req := request{
			Cookies: c.Request.Cookies(),
			Headers: c.Request.Header,
		}

		if c.Request.Body != nil && c.Request.Form != nil {
			req.Form = c.Request.Form
		}

		requestBodyField := extractRequestBody(c, config, fullPath)
		c.Next()
		sanitizeRequest(req, defaultSanitizedFieldNames)
		if len(req.Cookies) > 0 {
			cookies := make([]string, 0, len(req.Cookies))
			for _, v := range req.Cookies {
				cookies = append(cookies, v.String())
			}

			req.Headers[cookieHeader] = []string{strings.Join(cookies, "; ")}
		}

		// If skip contains the current path or the path matches the regex, skip it.
		skip := mapStringSlice(config.SkipPath)
		if _, ok := skip[fullPath]; ok ||
			(config.SkipPathRegexp != nil &&
				config.SkipPathRegexp.MatchString(fullPath) ||
				(config.SkipBodyPathRegexp != nil &&
					config.SkipBodyPathRegexp.MatchString(fullPath))) {
			return
		}

		end := time.Now().UTC()
		duration := end.Sub(start)
		msg := "Request"
		if len(c.Errors) > 0 {
			msg = c.Errors.String()
		}

		traceID := extractTraceParent(c)
		sanitizeHeaders(c.Writer.Header(), defaultSanitizedFieldNames)

		logger := config.Logger.WithFields(
			logrus.Fields{
				"request.method":     c.Request.Method,
				"request.path":       fullPath,
				"request.ip":         c.ClientIP(),
				"request.user-agent": c.Request.UserAgent(),
				"request.headers":    req.Headers,
				"request.body":       requestBodyField,
				"trace.id":           traceID,
				"response.headers":   c.Writer.Header(),
				"response.status":    c.Writer.Status(),
				"duration":           duration,
			},
		)

		switch {
		case isWarning(c):
			logger.Warn(msg)
		case isError(c):
			logger.Error(msg)
		default:
			logger.Info(msg)
		}
	}
}

// StartSpan starts an "external.grpc" span under the transaction in ctx,
// returns the created span and the context with the traceparent header matadata.
func StartSpan(ctx context.Context, name string) (*apm.Span, context.Context) {
	span, ctx := apm.StartSpan(ctx, name, "external.grpc")
	if span.Dropped() {
		return span, ctx
	}

	traceparentValue := apmhttp.FormatTraceparentHeader(span.TraceContext())
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		md = metadata.Pairs(strings.ToLower(apmhttp.TraceparentHeader), traceparentValue)
	} else {
		md = md.Copy()
		md.Set(strings.ToLower(apmhttp.TraceparentHeader), traceparentValue)
	}

	return span, metadata.NewOutgoingContext(ctx, md)
}

// sanitizeRequest sanitizes HTTP request data, redacting the
// values of cookies, headers and forms whose corresponding keys
// match any of the given wildcard patterns.
func sanitizeRequest(r request, matchers []string) {
	for _, m := range matchers {
		reg := regexp.MustCompile(m)
		for _, c := range r.Cookies {
			if !reg.MatchString(strings.ToLower(c.Name)) {
				continue
			}
			c.Value = redacted
		}
	}

	sanitizeHeaders(r.Headers, matchers)

	for _, m := range matchers {
		reg := regexp.MustCompile(m)
		for k, v := range r.Form {
			if !reg.MatchString(k) {
				continue
			}
			for i := range v {
				v[i] = redacted
			}
		}
	}
}

func sanitizeHeaders(headers http.Header, matchers []string) {
	for _, m := range matchers {
		reg := regexp.MustCompile(m)
		for k, v := range headers {
			if !reg.MatchString(strings.ToLower(k)) || len(v) == 0 {
				continue
			}
			headers[k] = headers[k][:1]
			headers[k][0] = redacted
		}
	}
}

func extractTraceParent(c *gin.Context) string {
	// If apmhttp.TraceparentHeader is present in request's headers
	// then parse the trace id and return it.
	if values := c.Request.Header[apmhttp.TraceparentHeader]; len(values) == 1 && values[0] != "" {
		if traceContext, err := apmhttp.ParseTraceparentHeader(values[0]); err == nil {
			return traceContext.Trace.String()
		}
	}

	// If apmhttp.TraceparentHeader is not present then return the created
	// transaction's trace id from its context.
	tx := apm.TransactionFromContext(c.Request.Context())
	return tx.TraceContext().Trace.String()
}

func mapStringSlice(s []string) map[string]struct{} {
	var mappedSlice map[string]struct{}
	if length := len(s); length > 0 {
		mappedSlice = make(map[string]struct{}, length)
		for _, v := range s {
			mappedSlice[v] = struct{}{}
		}
	}

	return mappedSlice
}

func extractRequestBody(c *gin.Context, config *Config, fullPath string) string {
	skipBody := mapStringSlice(config.SkipBodyPath)
	requestBodyField := ""
	if _, ok := skipBody[fullPath]; !ok ||
		!(config.SkipPathRegexp != nil &&
			config.SkipPathRegexp.MatchString(fullPath) ||
			(config.SkipBodyPathRegexp != nil &&
				config.SkipBodyPathRegexp.MatchString(fullPath))) {
		if c.Request.ContentLength > 0 &&
			c.Request.ContentLength <= 1<<20 {
			var buf bytes.Buffer
			requestBody := io.TeeReader(c.Request.Body, &buf)

			if requestBody != nil {
				bodyBytes, err := ioutil.ReadAll(requestBody)
				c.Request.Body = ioutil.NopCloser(&buf)

				if err == nil {
					requestBodyField = string(bodyBytes)
				}
			}
		}
	}

	return requestBodyField
}

func getRequestFullPath(c *gin.Context) string {
	path := c.Request.URL.Path
	raw := c.Request.URL.RawQuery
	if raw != "" {
		path = path + "?" + raw
	}

	return path
}

func isWarning(c *gin.Context) bool {
	return c.Writer.Status() >= http.StatusBadRequest && c.Writer.Status() < http.StatusInternalServerError
}

func isError(c *gin.Context) bool {
	return c.Writer.Status() >= http.StatusInternalServerError
}

func setupConfig(config *Config) *Config {
	if config == nil {
		config = &Config{}
	}

	if config.Logger == nil {
		config.Logger = logrus.New()
	}

	return config
}
