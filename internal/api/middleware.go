package api

import (
	"github.com/gofiber/fiber/v2"
)

// MaxRequestBodyBytes is the hard ceiling enforced on every incoming request.
// 25 MB is generous enough for batch requests with a few medium-sized attachments
// while still protecting the VPS from memory-exhaustion attacks driven by
// deliberately oversized payloads.
const MaxRequestBodyBytes = 25 * 1024 * 1024 // 25 MB

// RequestSizeLimit returns a Fiber middleware that rejects any request whose
// declared Content-Length or streamed body exceeds MaxRequestBodyBytes.
//
// Why not rely on fiber.Config.BodyLimit alone?
// fiber.Config.BodyLimit only guards the body parser; a raw streaming handler
// could still receive an unbounded body. This middleware sits at the router
// level, so every handler—present and future—is protected uniformly.
//
// Usage (in cmd/api/main.go):
//
//	app.Use(api.RequestSizeLimit())
func RequestSizeLimit() fiber.Handler {
	return func(c *fiber.Ctx) error {
		// Fast-path: reject early if Content-Length header is already over budget.
		// This avoids reading a single byte of an oversized payload.
		if c.Request().Header.ContentLength() > MaxRequestBodyBytes {
			return c.Status(fiber.StatusRequestEntityTooLarge).JSON(fiber.Map{
				"error": "request body exceeds the 25 MB limit",
			})
		}

		// Slow-path: for chunked / streamed requests that omit Content-Length,
		// check the actual body length after Fiber has buffered it.
		// Fiber buffers the full body before routing, so len(c.Body()) is safe.
		if len(c.Body()) > MaxRequestBodyBytes {
			return c.Status(fiber.StatusRequestEntityTooLarge).JSON(fiber.Map{
				"error": "request body exceeds the 25 MB limit",
			})
		}

		return c.Next()
	}
}
