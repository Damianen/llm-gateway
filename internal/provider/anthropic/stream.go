package anthropic

import (
	"context"

	"github.com/Damianen/llm-gateway/internal/provider"
)

// Stream performs a streaming completion. Implemented in Phase 4.
func (c *Client) Stream(ctx context.Context, req *provider.Request) (provider.Stream, error) {
	return nil, &provider.UpstreamError{Provider: providerType, StatusCode: 0,
		Message: "streaming not implemented yet"}
}
