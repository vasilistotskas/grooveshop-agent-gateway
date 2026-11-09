package adminmcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
)

type WhoamiOut struct {
	Email     string              `json:"email"`
	Role      string              `json:"role"`
	RoleLabel string              `json:"roleLabel"`
	Store     string              `json:"store"`
	Domain    string              `json:"domain"`
	Scopes    []django.StaffScope `json:"scopes"`
}

func (h *handlers) whoami(
	ctx context.Context, _ *mcp.CallToolRequest, _ struct{},
) (*mcp.CallToolResult, WhoamiOut, error) {
	_, _, staff, err := h.call(ctx, "")
	if err != nil {
		return nil, WhoamiOut{}, err
	}
	p := staff.Profile
	return nil, WhoamiOut{
		Email:     p.Email,
		Role:      p.Role,
		RoleLabel: p.RoleDisplay,
		Store:     p.StoreName,
		Domain:    p.StoreDomain,
		Scopes:    p.Scopes,
	}, nil
}

type ListOrdersIn struct {
	Status        string `json:"status,omitempty" jsonschema:"order status, e.g. PROCESSING"`
	PaymentStatus string `json:"paymentStatus,omitempty" jsonschema:"payment status, e.g. COMPLETED or PENDING"`
	PlacedFrom    string `json:"placedFrom,omitempty" jsonschema:"first day placed, YYYY-MM-DD"`
	PlacedTo      string `json:"placedTo,omitempty" jsonschema:"last day placed, YYYY-MM-DD"`
	Search        string `json:"search,omitempty" jsonschema:"order id, customer email or name"`
	Limit         int    `json:"limit,omitempty" jsonschema:"how many, capped by the store"`
}

type ListOrdersOut struct {
	Orders []django.StaffOrder `json:"orders"`
}

func (h *handlers) listOrders(
	ctx context.Context, _ *mcp.CallToolRequest, in ListOrdersIn,
) (*mcp.CallToolResult, ListOrdersOut, error) {
	t, lang, staff, err := h.call(ctx, ScopeOrders)
	if err != nil {
		return nil, ListOrdersOut{}, err
	}
	orders, err := h.deps.Django.StaffOrders(ctx, t.Domain, lang,
		staff.Bearer, django.StaffOrdersQuery{
			Status:        in.Status,
			PaymentStatus: in.PaymentStatus,
			PlacedFrom:    in.PlacedFrom,
			PlacedTo:      in.PlacedTo,
			Search:        in.Search,
			Limit:         in.Limit,
		})
	if err != nil {
		return nil, ListOrdersOut{}, upstreamErr(err, "no orders found")
	}
	if orders == nil {
		orders = []django.StaffOrder{}
	}
	return nil, ListOrdersOut{Orders: orders}, nil
}

type GetOrderIn struct {
	OrderID int64 `json:"orderId" jsonschema:"the order id from list_orders"`
}

// OrderDetail is the order with the customer's note marked untrusted.
type OrderDetail struct {
	django.StaffOrder
	ItemsTotal      string                  `json:"itemsTotal"`
	Shipping        string                  `json:"shipping"`
	PaymentFee      string                  `json:"paymentFee"`
	Discount        string                  `json:"discount"`
	ShippingCarrier string                  `json:"shippingCarrier"`
	TrackingNumber  string                  `json:"trackingNumber"`
	City            string                  `json:"city"`
	Country         *string                 `json:"country"`
	CustomerNote    *Untrusted              `json:"customerNote,omitempty"`
	Lines           []django.StaffOrderLine `json:"lines"`
}

func (h *handlers) getOrder(
	ctx context.Context, _ *mcp.CallToolRequest, in GetOrderIn,
) (*mcp.CallToolResult, OrderDetail, error) {
	t, lang, staff, err := h.call(ctx, ScopeOrders)
	if err != nil {
		return nil, OrderDetail{}, err
	}
	o, err := h.deps.Django.StaffOrder(ctx, t.Domain, lang, staff.Bearer,
		in.OrderID)
	if err != nil {
		return nil, OrderDetail{}, upstreamErr(err,
			"no order with that id in this store; find it with list_orders")
	}
	return nil, OrderDetail{
		StaffOrder:      o.StaffOrder,
		ItemsTotal:      o.ItemsTotal,
		Shipping:        o.Shipping,
		PaymentFee:      o.PaymentFee,
		Discount:        o.Discount,
		ShippingCarrier: o.ShippingCarrier,
		TrackingNumber:  o.TrackingNumber,
		City:            o.City,
		Country:         o.Country,
		CustomerNote:    untrusted("customer", o.CustomerNotes),
		Lines:           o.Lines,
	}, nil
}

type ReportIn struct {
	Start  string `json:"start,omitempty" jsonschema:"first day, YYYY-MM-DD; default 30 days before end"`
	End    string `json:"end,omitempty" jsonschema:"last day, YYYY-MM-DD; default today"`
	Bucket string `json:"bucket,omitempty" jsonschema:"day, week, month or quarter; omitted: one row for the range"`
}

func (in ReportIn) query() django.StaffReportQuery {
	return django.StaffReportQuery{
		Start: in.Start, End: in.End, Bucket: in.Bucket,
	}
}

func (h *handlers) salesReport(
	ctx context.Context, _ *mcp.CallToolRequest, in ReportIn,
) (*mcp.CallToolResult, django.StaffSalesReport, error) {
	t, lang, staff, err := h.call(ctx, ScopeSales)
	if err != nil {
		return nil, django.StaffSalesReport{}, err
	}
	report, err := h.deps.Django.StaffSales(ctx, t.Domain, lang,
		staff.Bearer, in.query())
	if err != nil {
		return nil, django.StaffSalesReport{}, upstreamErr(err, "no report")
	}
	return nil, *report, nil
}

func (h *handlers) profitReport(
	ctx context.Context, _ *mcp.CallToolRequest, in ReportIn,
) (*mcp.CallToolResult, django.StaffProfitReport, error) {
	t, lang, staff, err := h.call(ctx, ScopeProfit)
	if err != nil {
		return nil, django.StaffProfitReport{}, err
	}
	report, err := h.deps.Django.StaffProfit(ctx, t.Domain, lang,
		staff.Bearer, in.query())
	if err != nil {
		return nil, django.StaffProfitReport{}, upstreamErr(err, "no report")
	}
	return nil, *report, nil
}

type ListProductsIn struct {
	Search   string `json:"search,omitempty" jsonschema:"name, SKU or product id"`
	LowStock bool   `json:"lowStock,omitempty" jsonschema:"only products at or under their low-stock threshold"`
	Limit    int    `json:"limit,omitempty" jsonschema:"how many, capped by the store"`
}

type ListProductsOut struct {
	Products []django.StaffProduct `json:"products"`
}

func (h *handlers) listProducts(
	ctx context.Context, _ *mcp.CallToolRequest, in ListProductsIn,
) (*mcp.CallToolResult, ListProductsOut, error) {
	t, lang, staff, err := h.call(ctx, ScopeCatalog)
	if err != nil {
		return nil, ListProductsOut{}, err
	}
	products, err := h.deps.Django.StaffProducts(ctx, t.Domain, lang,
		staff.Bearer, in.Search, in.LowStock, in.Limit)
	if err != nil {
		return nil, ListProductsOut{}, upstreamErr(err, "no products found")
	}
	if products == nil {
		products = []django.StaffProduct{}
	}
	return nil, ListProductsOut{Products: products}, nil
}

type SearchIn struct {
	Search string `json:"search,omitempty" jsonschema:"email or name"`
	Limit  int    `json:"limit,omitempty" jsonschema:"how many, capped by the store"`
}

type FindCustomersOut struct {
	Customers []django.StaffCustomer `json:"customers"`
}

func (h *handlers) findCustomers(
	ctx context.Context, _ *mcp.CallToolRequest, in SearchIn,
) (*mcp.CallToolResult, FindCustomersOut, error) {
	t, lang, staff, err := h.call(ctx, ScopeCustomers)
	if err != nil {
		return nil, FindCustomersOut{}, err
	}
	customers, err := h.deps.Django.StaffCustomers(ctx, t.Domain, lang,
		staff.Bearer, in.Search, in.Limit)
	if err != nil {
		return nil, FindCustomersOut{}, upstreamErr(err, "no customers found")
	}
	if customers == nil {
		customers = []django.StaffCustomer{}
	}
	return nil, FindCustomersOut{Customers: customers}, nil
}

type ListReviewsIn struct {
	Status string `json:"status,omitempty" jsonschema:"NEW (waiting), TRUE (approved) or FALSE (rejected)"`
	Limit  int    `json:"limit,omitempty" jsonschema:"how many, capped by the store"`
}

// Review is a review with its comment marked untrusted.
type Review struct {
	ID            int64      `json:"id"`
	ProductID     int64      `json:"productId"`
	ProductName   string     `json:"productName"`
	Rating        int64      `json:"rating"`
	Status        string     `json:"status"`
	StatusDisplay string     `json:"statusDisplay"`
	Published     bool       `json:"published"`
	CreatedAt     string     `json:"createdAt"`
	Comment       *Untrusted `json:"comment,omitempty"`
}

type ListReviewsOut struct {
	Reviews []Review `json:"reviews"`
}

func (h *handlers) listReviews(
	ctx context.Context, _ *mcp.CallToolRequest, in ListReviewsIn,
) (*mcp.CallToolResult, ListReviewsOut, error) {
	t, lang, staff, err := h.call(ctx, ScopeReviews)
	if err != nil {
		return nil, ListReviewsOut{}, err
	}
	rows, err := h.deps.Django.StaffReviews(ctx, t.Domain, lang,
		staff.Bearer, in.Status, in.Limit)
	if err != nil {
		return nil, ListReviewsOut{}, upstreamErr(err, "no reviews found")
	}
	out := ListReviewsOut{Reviews: make([]Review, 0, len(rows))}
	for _, r := range rows {
		out.Reviews = append(out.Reviews, Review{
			ID:            r.ID,
			ProductID:     r.ProductID,
			ProductName:   r.ProductName,
			Rating:        r.Rating,
			Status:        r.Status,
			StatusDisplay: r.StatusDisplay,
			Published:     r.Published,
			CreatedAt:     r.CreatedAt,
			Comment:       untrusted("customer", r.Comment),
		})
	}
	return nil, out, nil
}

type LimitIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"how many, capped by the store"`
}

// Message is a contact message with what the sender wrote marked
// untrusted.
type Message struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	Email     string     `json:"email"`
	Subject   *Untrusted `json:"subject,omitempty"`
	CreatedAt string     `json:"createdAt"`
	Message   *Untrusted `json:"message"`
}

type ListMessagesOut struct {
	Messages []Message `json:"messages"`
}

func (h *handlers) listMessages(
	ctx context.Context, _ *mcp.CallToolRequest, in LimitIn,
) (*mcp.CallToolResult, ListMessagesOut, error) {
	t, lang, staff, err := h.call(ctx, ScopeMessages)
	if err != nil {
		return nil, ListMessagesOut{}, err
	}
	rows, err := h.deps.Django.StaffMessages(ctx, t.Domain, lang,
		staff.Bearer, in.Limit)
	if err != nil {
		return nil, ListMessagesOut{}, upstreamErr(err, "no messages found")
	}
	out := ListMessagesOut{Messages: make([]Message, 0, len(rows))}
	for _, m := range rows {
		out.Messages = append(out.Messages, Message{
			ID:        m.ID,
			Name:      m.Name,
			Email:     m.Email,
			Subject:   untrusted("sender", m.Subject),
			CreatedAt: m.CreatedAt,
			Message:   untrusted("sender", m.Message),
		})
	}
	return nil, out, nil
}
