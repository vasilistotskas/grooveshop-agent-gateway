package chat

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// cartTools mutate the shopper's cart; a successful call flags the widget
// to refresh its cart store.
var cartTools = map[string]bool{
	"create_cart":      true,
	"add_to_cart":      true,
	"update_cart_item": true,
	"remove_cart_item": true,
}

// bridge is a per-turn in-process MCP client over the shared tool server.
// Claude's tool definitions are derived from the server's own schemas, so
// the chatbot and external MCP agents can never drift apart.
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

// accountTools need an OAuth-linked agent credential on the MCP HTTP
// request — the chat widget's shopper authenticates via the storefront
// session instead, so these tools are excluded from the bot's toolset
// (the storefront UI already shows orders and loyalty).
var accountTools = map[string]bool{
	"my_orders":         true,
	"my_loyalty_points": true,
}

// tools converts every MCP tool into an Anthropic tool whose handler calls
// back through the MCP session. Handlers run concurrently within a turn,
// hence the mutex around cart state.
func (b *bridge) tools(ctx context.Context) ([]anthropic.BetaTool, error) {
	list, err := b.session.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	tools := make([]anthropic.BetaTool, 0, len(list.Tools))
	for _, t := range list.Tools {
		if accountTools[t.Name] {
			continue
		}
		schemaJSON, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, err
		}
		name := t.Name
		tool, err := toolrunner.NewBetaToolFromBytes(
			name, t.Description, schemaJSON,
			func(callCtx context.Context, input map[string]any) (
				anthropic.BetaToolResultBlockParamContentUnion, error,
			) {
				return b.call(callCtx, name, input)
			},
		)
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

func (b *bridge) call(
	ctx context.Context, name string, input map[string]any,
) (anthropic.BetaToolResultBlockParamContentUnion, error) {
	res, err := b.session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: input,
	})
	if err != nil {
		return anthropic.BetaToolResultBlockParamContentUnion{}, err
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
	return anthropic.BetaToolResultBlockParamContentUnion{
		OfText: &anthropic.BetaTextBlockParam{Text: text},
	}, nil
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
