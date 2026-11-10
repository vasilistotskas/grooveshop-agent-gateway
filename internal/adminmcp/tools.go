package adminmcp

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/django"
	"github.com/vasilistotskas/grooveshop-agent-gateway/internal/money"
)

// amount renders an amount Django sent as a JSON number as the
// two-place decimal string a tool answers with ("12.5" → "12.50").
// Django's amounts all carry two places; one that does not parse is
// passed on as sent.
func amount(n json.Number) string {
	minor, err := money.MinorUnits(n.String())
	if err != nil {
		return n.String()
	}
	return money.Format(minor)
}

// ratio renders a figure Django leaves null when it has no base to
// divide by (a margin on no revenue): nil then, the amount otherwise.
func ratio(n json.Number) *string {
	if n == "" {
		return nil
	}
	out := amount(n)
	return &out
}

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

// Order is an order row with the name its customer typed marked
// untrusted.
type Order struct {
	ID            int64      `json:"id"`
	UUID          string     `json:"uuid"`
	Status        string     `json:"status"`
	StatusDisplay string     `json:"statusDisplay"`
	PaymentStatus string     `json:"paymentStatus"`
	PaymentMethod string     `json:"paymentMethod"`
	PlacedAt      string     `json:"placedAt"`
	ShippedAt     *string    `json:"shippedAt"`
	CustomerName  *Untrusted `json:"customerName,omitempty"`
	CustomerEmail string     `json:"customerEmail"`
	Total         string     `json:"total"`
	Currency      string     `json:"currency"`
}

func orderOf(o django.StaffOrder) Order {
	return Order{
		ID:            o.ID,
		UUID:          o.UUID,
		Status:        o.Status,
		StatusDisplay: o.StatusDisplay,
		PaymentStatus: o.PaymentStatus,
		PaymentMethod: o.PaymentMethod,
		PlacedAt:      o.PlacedAt,
		ShippedAt:     o.ShippedAt,
		CustomerName:  untrusted("customer", o.CustomerName),
		CustomerEmail: o.CustomerEmail,
		Total:         amount(o.Total),
		Currency:      o.Currency,
	}
}

type ListOrdersOut struct {
	Orders []Order `json:"orders"`
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
		return nil, ListOrdersOut{}, h.upstreamErr(ctx, err, "no orders found")
	}
	out := ListOrdersOut{Orders: make([]Order, 0, len(orders))}
	for _, o := range orders {
		out.Orders = append(out.Orders, orderOf(o))
	}
	return nil, out, nil
}

type GetOrderIn struct {
	OrderID int64 `json:"orderId" jsonschema:"the order id from list_orders"`
}

// OrderDetail is the order with what its customer typed (name, city,
// note) marked untrusted.
type OrderDetail struct {
	Order
	ItemsTotal      string      `json:"itemsTotal"`
	Shipping        string      `json:"shipping"`
	PaymentFee      string      `json:"paymentFee"`
	Discount        string      `json:"discount"`
	ShippingCarrier string      `json:"shippingCarrier"`
	TrackingNumber  string      `json:"trackingNumber"`
	City            *Untrusted  `json:"city,omitempty"`
	Country         *string     `json:"country"`
	CustomerNote    *Untrusted  `json:"customerNote,omitempty"`
	Lines           []OrderLine `json:"lines"`
}

// OrderLine is one line of an order.
type OrderLine struct {
	ProductID *int64 `json:"productId"`
	Name      string `json:"name"`
	SKU       string `json:"sku"`
	Quantity  int64  `json:"quantity"`
	UnitPrice string `json:"unitPrice"`
	LineTotal string `json:"lineTotal"`
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
		return nil, OrderDetail{}, h.upstreamErr(ctx, err,
			"no order with that id in this store; find it with list_orders")
	}
	lines := make([]OrderLine, 0, len(o.Lines))
	for _, l := range o.Lines {
		lines = append(lines, OrderLine{
			ProductID: l.ProductID,
			Name:      l.Name,
			SKU:       l.SKU,
			Quantity:  l.Quantity,
			UnitPrice: amount(l.UnitPrice),
			LineTotal: amount(l.LineTotal),
		})
	}
	return nil, OrderDetail{
		Order:           orderOf(o.StaffOrder),
		ItemsTotal:      amount(o.ItemsTotal),
		Shipping:        amount(o.Shipping),
		PaymentFee:      amount(o.PaymentFee),
		Discount:        amount(o.Discount),
		ShippingCarrier: o.ShippingCarrier,
		TrackingNumber:  o.TrackingNumber,
		City:            untrusted("customer", o.City),
		Country:         o.Country,
		CustomerNote:    untrusted("customer", o.CustomerNotes),
		Lines:           lines,
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

// SalesRow is the takings of one period, or of the whole range.
type SalesRow struct {
	Period             *string `json:"period"`
	Orders             int64   `json:"orders"`
	Units              int64   `json:"units"`
	SalesNet           string  `json:"salesNet"`
	SalesGross         string  `json:"salesGross"`
	ReturnsNet         string  `json:"returnsNet"`
	Turnover           string  `json:"turnover"`
	VAT                string  `json:"vat"`
	AverageOrderValue  string  `json:"averageOrderValue"`
	AwaitingCollection string  `json:"awaitingCollection"`
}

func salesRowOf(r django.StaffSalesRow) SalesRow {
	return SalesRow{
		Period:             r.Period,
		Orders:             r.Orders,
		Units:              r.Units,
		SalesNet:           amount(r.SalesNet),
		SalesGross:         amount(r.SalesGross),
		ReturnsNet:         amount(r.ReturnsNet),
		Turnover:           amount(r.Turnover),
		VAT:                amount(r.VAT),
		AverageOrderValue:  amount(r.AverageOrderValue),
		AwaitingCollection: amount(r.AwaitingCollection),
	}
}

type SalesReport struct {
	Start    string     `json:"start"`
	End      string     `json:"end"`
	Bucket   *string    `json:"bucket"`
	Currency string     `json:"currency"`
	Rows     []SalesRow `json:"rows"`
	Total    SalesRow   `json:"total"`
}

func (h *handlers) salesReport(
	ctx context.Context, _ *mcp.CallToolRequest, in ReportIn,
) (*mcp.CallToolResult, SalesReport, error) {
	t, lang, staff, err := h.call(ctx, ScopeSales)
	if err != nil {
		return nil, SalesReport{}, err
	}
	report, err := h.deps.Django.StaffSales(ctx, t.Domain, lang,
		staff.Bearer, in.query())
	if err != nil {
		return nil, SalesReport{}, h.upstreamErr(ctx, err, "no report")
	}
	out := SalesReport{
		Start:    report.Start,
		End:      report.End,
		Bucket:   report.Bucket,
		Currency: report.Currency,
		Rows:     make([]SalesRow, 0, len(report.Rows)),
		Total:    salesRowOf(report.Total),
	}
	for _, r := range report.Rows {
		out.Rows = append(out.Rows, salesRowOf(r))
	}
	return nil, out, nil
}

// ProfitRow is the profit of one period, or of the whole range. A
// margin or coverage with nothing to divide by is null.
type ProfitRow struct {
	Period          *string `json:"period"`
	Revenue         string  `json:"revenue"`
	COGS            string  `json:"cogs"`
	GrossProfit     string  `json:"grossProfit"`
	GrossMargin     *string `json:"grossMargin"`
	PaymentFees     string  `json:"paymentFees"`
	DeliveryCosts   string  `json:"deliveryCosts"`
	OtherExpenses   string  `json:"otherExpenses"`
	OperatingProfit string  `json:"operatingProfit"`
	CostCoverage    *string `json:"costCoverage"`
	FeeCoverage     *string `json:"feeCoverage"`
}

func profitRowOf(r django.StaffProfitRow) ProfitRow {
	return ProfitRow{
		Period:          r.Period,
		Revenue:         amount(r.Revenue),
		COGS:            amount(r.COGS),
		GrossProfit:     amount(r.GrossProfit),
		GrossMargin:     ratio(r.GrossMargin),
		PaymentFees:     amount(r.PaymentFees),
		DeliveryCosts:   amount(r.DeliveryCosts),
		OtherExpenses:   amount(r.OtherExpenses),
		OperatingProfit: amount(r.OperatingProfit),
		CostCoverage:    ratio(r.CostCoverage),
		FeeCoverage:     ratio(r.FeeCoverage),
	}
}

type ProfitReport struct {
	Start    string      `json:"start"`
	End      string      `json:"end"`
	Bucket   *string     `json:"bucket"`
	Currency string      `json:"currency"`
	Rows     []ProfitRow `json:"rows"`
	Total    ProfitRow   `json:"total"`
}

func (h *handlers) profitReport(
	ctx context.Context, _ *mcp.CallToolRequest, in ReportIn,
) (*mcp.CallToolResult, ProfitReport, error) {
	t, lang, staff, err := h.call(ctx, ScopeProfit)
	if err != nil {
		return nil, ProfitReport{}, err
	}
	report, err := h.deps.Django.StaffProfit(ctx, t.Domain, lang,
		staff.Bearer, in.query())
	if err != nil {
		return nil, ProfitReport{}, h.upstreamErr(ctx, err, "no report")
	}
	out := ProfitReport{
		Start:    report.Start,
		End:      report.End,
		Bucket:   report.Bucket,
		Currency: report.Currency,
		Rows:     make([]ProfitRow, 0, len(report.Rows)),
		Total:    profitRowOf(report.Total),
	}
	for _, r := range report.Rows {
		out.Rows = append(out.Rows, profitRowOf(r))
	}
	return nil, out, nil
}

type ListProductsIn struct {
	Search   string `json:"search,omitempty" jsonschema:"name, SKU or product id"`
	LowStock bool   `json:"lowStock,omitempty" jsonschema:"only products at or under their low-stock threshold"`
	Limit    int    `json:"limit,omitempty" jsonschema:"how many, capped by the store"`
}

// Product is a product row with its prices and stock.
type Product struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	SKU               string `json:"sku"`
	Price             string `json:"price"`
	FinalPrice        string `json:"finalPrice"`
	Currency          string `json:"currency"`
	Stock             int64  `json:"stock"`
	LowStockThreshold int64  `json:"lowStockThreshold"`
	LowStock          bool   `json:"lowStock"`
	Published         bool   `json:"published"`
}

type ListProductsOut struct {
	Products []Product `json:"products"`
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
		return nil, ListProductsOut{}, h.upstreamErr(ctx, err, "no products found")
	}
	out := ListProductsOut{Products: make([]Product, 0, len(products))}
	for _, p := range products {
		out.Products = append(out.Products, Product{
			ID:                p.ID,
			Name:              p.Name,
			SKU:               p.SKU,
			Price:             amount(p.Price),
			FinalPrice:        amount(p.FinalPrice),
			Currency:          p.Currency,
			Stock:             p.Stock,
			LowStockThreshold: p.LowStockThreshold,
			LowStock:          p.LowStock,
			Published:         p.Published,
		})
	}
	return nil, out, nil
}

type SearchIn struct {
	Search string `json:"search,omitempty" jsonschema:"email or name"`
	Limit  int    `json:"limit,omitempty" jsonschema:"how many, capped by the store"`
}

// Customer is a customer with the name they typed marked untrusted.
type Customer struct {
	ID          int64      `json:"id"`
	Name        *Untrusted `json:"name,omitempty"`
	Email       string     `json:"email"`
	JoinedAt    string     `json:"joinedAt"`
	IsActive    bool       `json:"isActive"`
	Orders      int64      `json:"orders"`
	LastOrderAt *string    `json:"lastOrderAt"`
}

type FindCustomersOut struct {
	Customers []Customer `json:"customers"`
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
		return nil, FindCustomersOut{}, h.upstreamErr(ctx, err, "no customers found")
	}
	out := FindCustomersOut{Customers: make([]Customer, 0, len(customers))}
	for _, c := range customers {
		out.Customers = append(out.Customers, Customer{
			ID:          c.ID,
			Name:        untrusted("customer", c.Name),
			Email:       c.Email,
			JoinedAt:    c.JoinedAt,
			IsActive:    c.IsActive,
			Orders:      c.Orders,
			LastOrderAt: c.LastOrderAt,
		})
	}
	return nil, out, nil
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
		return nil, ListReviewsOut{}, h.upstreamErr(ctx, err, "no reviews found")
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
	Name      *Untrusted `json:"name,omitempty"`
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
		return nil, ListMessagesOut{}, h.upstreamErr(ctx, err, "no messages found")
	}
	out := ListMessagesOut{Messages: make([]Message, 0, len(rows))}
	for _, m := range rows {
		out.Messages = append(out.Messages, Message{
			ID:        m.ID,
			Name:      untrusted("sender", m.Name),
			Email:     m.Email,
			Subject:   untrusted("sender", m.Subject),
			CreatedAt: m.CreatedAt,
			Message:   untrusted("sender", m.Message),
		})
	}
	return nil, out, nil
}
