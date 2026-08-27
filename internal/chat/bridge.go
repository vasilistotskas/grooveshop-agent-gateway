package chat

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

// cartTools mutate the shopper's cart; a successful call flags the widget
// to refresh its cart store.
var cartTools = map[string]bool{
	"create_cart":      true,
	"add_to_cart":      true,
	"update_cart_item": true,
	"remove_cart_item": true,
}

// accountTools need an OAuth-linked agent credential on the MCP HTTP
// request — the chat widget's shopper authenticates via the storefront
// session instead, so these tools are excluded from the bot's toolset
// (the storefront UI already shows orders and loyalty).
// checkoutTools are the UCP checkout capability's canonical tools. They
// are withheld from the chatbot: their arguments are shaped for a
// platform generating calls from the OpenRPC document, not for a
// conversational model, and the shopping assistant is meant to hand the
// buyer a checkout link rather than place orders itself.
var checkoutTools = map[string]bool{
	"create_checkout":   true,
	"get_checkout":      true,
	"update_checkout":   true,
	"complete_checkout": true,
	"cancel_checkout":   true,
}

var accountTools = map[string]bool{
	"my_orders":         true,
	"my_loyalty_points": true,
	"my_favourites":     true,
}

// bridge is a per-turn in-process MCP client over the shared tool server.
// The model's tool definitions are derived from the server's own schemas,
// so the chatbot and external MCP agents can never drift apart.
type bridge struct {
	session *mcp.ClientSession

	mu          sync.Mutex
	cartID      string
	cartMutated bool
}

// newBridge connects an MCP client to the shared server over in-memory
// transports. ctx must carry the tenant — it becomes the server session's
// base context, exactly like the HTTP transport path.
func newBridge(ctx context.Context, server *mcp.Server) (*bridge, error) {
	serverT, clientT := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverT, nil); err != nil {
		return nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "grooveshop-chat",
		Version: "internal",
	}, nil)
	session, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		return nil, err
	}
	return &bridge{session: session}, nil
}

func (b *bridge) close() {
	_ = b.session.Close()
}

// tools converts every MCP tool into an OpenAI-protocol function tool.
// The MCP input schema is already draft-07-compatible JSON Schema, which
// is what the chat-completions `parameters` field expects.
func (b *bridge) tools(
	ctx context.Context,
) ([]openai.ChatCompletionToolUnionParam, error) {
	list, err := b.session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	tools := make([]openai.ChatCompletionToolUnionParam, 0, len(list.Tools))
	for _, t := range list.Tools {
		if accountTools[t.Name] || checkoutTools[t.Name] {
			continue
		}
		raw, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, err
		}
		var params shared.FunctionParameters
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		tools = append(tools, openai.ChatCompletionFunctionTool(
			shared.FunctionDefinitionParam{
				Name:        t.Name,
				Description: openai.String(t.Description),
				Parameters:  params,
			}))
	}
	return tools, nil
}

// call executes one tool through the MCP session and renders the result
// as the plain text a tool-role message carries. Business failures come
// back as "ERROR: …" text so the model can react (the MCP server never
// turns them into protocol errors).
func (b *bridge) call(
	ctx context.Context, name string, input map[string]any,
) (string, error) {
	res, err := b.session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: input,
	})
	if err != nil {
		return "", err
	}

	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	if res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			parts = append(parts, string(raw))
			if !res.IsError {
				b.recordCartState(name, raw)
			}
		}
	}
	text := strings.Join(parts, "\n")
	if res.IsError {
		text = "ERROR: " + text
	}
	return text, nil
}

// recordCartState tracks the active cart id and whether this turn changed
// the cart, so the widget can adopt/refresh it.
func (b *bridge) recordCartState(tool string, structured []byte) {
	if !cartTools[tool] {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cartMutated = true
	var payload struct {
		CartID string `json:"cartId"`
	}
	if err := json.Unmarshal(structured, &payload); err == nil &&
		payload.CartID != "" {
		b.cartID = payload.CartID
	}
}

func (b *bridge) cartState() (cartID string, mutated bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.cartID, b.cartMutated
}
