package django

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// The store-admin surface (/api/v1/agent/staff/*) serves /mcp/admin. It
// accepts only access tokens from the platform's staff issuer, bound to
// the store's /mcp/admin, held by someone on the store's team now; the
// gateway forwards the token verbatim. 401 unwraps to ErrUnauthorized
// (connect again), 403 to ErrForbidden (a scope the app was not granted,
// or a permission the person's role lacks). Amounts are decimal strings,
// as Django renders them.

// StaffProfile is who is connected, to which store, and what the app may
// read: each scope, whether it was granted at consent and whether the
// person's role opens it today.
type StaffProfile struct {
	Email       string       `json:"email"`
	Role        string       `json:"role"`
	RoleDisplay string       `json:"roleDisplay"`
	StoreName   string       `json:"storeName"`
	StoreDomain string       `json:"storeDomain"`
	Scopes      []StaffScope `json:"scopes"`
}

type StaffScope struct {
	Scope       string `json:"scope"`
	Description string `json:"description"`
	Granted     bool   `json:"granted"`
	Allowed     bool   `json:"allowed"`
}

// Opens reports whether scope is both granted and open to the role.
func (p *StaffProfile) Opens(scope string) (granted, allowed bool) {
	for _, s := range p.Scopes {
		if s.Scope == scope {
			return s.Granted, s.Allowed
		}
	}
	return false, false
}

type StaffOrder struct {
	ID            int64   `json:"id"`
	UUID          string  `json:"uuid"`
	Status        string  `json:"status"`
	StatusDisplay string  `json:"statusDisplay"`
	PaymentStatus string  `json:"paymentStatus"`
	PaymentMethod string  `json:"paymentMethod"`
	PlacedAt      string  `json:"placedAt"`
	ShippedAt     *string `json:"shippedAt"`
	CustomerName  string  `json:"customerName"`
	CustomerEmail string  `json:"customerEmail"`
	Total         string  `json:"total"`
	Currency      string  `json:"currency"`
}

type StaffOrderLine struct {
	ProductID *int64 `json:"productId"`
	Name      string `json:"name"`
	SKU       string `json:"sku"`
	Quantity  int64  `json:"quantity"`
	UnitPrice string `json:"unitPrice"`
	LineTotal string `json:"lineTotal"`
}

type StaffOrderDetail struct {
	StaffOrder
	ItemsTotal      string           `json:"itemsTotal"`
	Shipping        string           `json:"shipping"`
	PaymentFee      string           `json:"paymentFee"`
	Discount        string           `json:"discount"`
	ShippingCarrier string           `json:"shippingCarrier"`
	TrackingNumber  string           `json:"trackingNumber"`
	City            string           `json:"city"`
	Country         *string          `json:"country"`
	CustomerNotes   string           `json:"customerNotes"`
	Lines           []StaffOrderLine `json:"lines"`
}

type StaffSalesRow struct {
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

type StaffSalesReport struct {
	Start    string          `json:"start"`
	End      string          `json:"end"`
	Bucket   *string         `json:"bucket"`
	Currency string          `json:"currency"`
	Rows     []StaffSalesRow `json:"rows"`
	Total    StaffSalesRow   `json:"total"`
}

type StaffProfitRow struct {
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

type StaffProfitReport struct {
	Start    string           `json:"start"`
	End      string           `json:"end"`
	Bucket   *string          `json:"bucket"`
	Currency string           `json:"currency"`
	Rows     []StaffProfitRow `json:"rows"`
	Total    StaffProfitRow   `json:"total"`
}

type StaffProduct struct {
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

type StaffCustomer struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Email       string  `json:"email"`
	JoinedAt    string  `json:"joinedAt"`
	IsActive    bool    `json:"isActive"`
	Orders      int64   `json:"orders"`
	LastOrderAt *string `json:"lastOrderAt"`
}

type StaffReview struct {
	ID            int64  `json:"id"`
	ProductID     int64  `json:"productId"`
	ProductName   string `json:"productName"`
	Rating        int64  `json:"rating"`
	Status        string `json:"status"`
	StatusDisplay string `json:"statusDisplay"`
	Published     bool   `json:"published"`
	CreatedAt     string `json:"createdAt"`
	Comment       string `json:"comment"`
}

type StaffMessage struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Subject   string `json:"subject"`
	CreatedAt string `json:"createdAt"`
	Message   string `json:"message"`
}

// StaffOrdersQuery filters list_orders; a zero field is not sent.
type StaffOrdersQuery struct {
	Status, PaymentStatus string
	PlacedFrom, PlacedTo  string
	Search                string
	Limit                 int
}

// StaffReportQuery is a report's range and bucket; a zero field is not
// sent and Django applies its default.
type StaffReportQuery struct {
	Start, End, Bucket string
}

func (c *Client) StaffMe(
	ctx context.Context, host, lang, token string,
) (*StaffProfile, error) {
	var out StaffProfile
	err := c.get(ctx, request{
		path:     "/agent/staff/me",
		host:     host,
		language: lang,
		headers:  bearerHeaders(token),
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) StaffOrders(
	ctx context.Context, host, lang, token string, p StaffOrdersQuery,
) ([]StaffOrder, error) {
	q := url.Values{}
	if p.Status != "" {
		q.Set("status", p.Status)
	}
	if p.PaymentStatus != "" {
		q.Set("paymentStatus", p.PaymentStatus)
	}
	if p.PlacedFrom != "" {
		q.Set("placedFrom", p.PlacedFrom)
	}
	if p.PlacedTo != "" {
		q.Set("placedTo", p.PlacedTo)
	}
	if p.Search != "" {
		q.Set("search", p.Search)
	}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	var out []StaffOrder
	err := c.get(ctx, request{
		path:     "/agent/staff/orders",
		host:     host,
		language: lang,
		query:    q,
		headers:  bearerHeaders(token),
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) StaffOrder(
	ctx context.Context, host, lang, token string, id int64,
) (*StaffOrderDetail, error) {
	var out StaffOrderDetail
	err := c.get(ctx, request{
		path:     "/agent/staff/orders/{id}",
		rawPath:  fmt.Sprintf("/agent/staff/orders/%d", id),
		host:     host,
		language: lang,
		headers:  bearerHeaders(token),
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) StaffSales(
	ctx context.Context, host, lang, token string, p StaffReportQuery,
) (*StaffSalesReport, error) {
	q := url.Values{}
	if p.Start != "" {
		q.Set("start", p.Start)
	}
	if p.End != "" {
		q.Set("end", p.End)
	}
	if p.Bucket != "" {
		q.Set("bucket", p.Bucket)
	}
	var out StaffSalesReport
	err := c.get(ctx, request{
		path:     "/agent/staff/sales",
		host:     host,
		language: lang,
		query:    q,
		headers:  bearerHeaders(token),
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) StaffProfit(
	ctx context.Context, host, lang, token string, p StaffReportQuery,
) (*StaffProfitReport, error) {
	q := url.Values{}
	if p.Start != "" {
		q.Set("start", p.Start)
	}
	if p.End != "" {
		q.Set("end", p.End)
	}
	if p.Bucket != "" {
		q.Set("bucket", p.Bucket)
	}
	var out StaffProfitReport
	err := c.get(ctx, request{
		path:     "/agent/staff/profit",
		host:     host,
		language: lang,
		query:    q,
		headers:  bearerHeaders(token),
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) StaffProducts(
	ctx context.Context, host, lang, token, search string,
	lowStock bool, limit int,
) ([]StaffProduct, error) {
	q := url.Values{}
	if search != "" {
		q.Set("search", search)
	}
	if lowStock {
		q.Set("lowStock", "true")
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out []StaffProduct
	err := c.get(ctx, request{
		path:     "/agent/staff/products",
		host:     host,
		language: lang,
		query:    q,
		headers:  bearerHeaders(token),
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) StaffCustomers(
	ctx context.Context, host, lang, token, search string, limit int,
) ([]StaffCustomer, error) {
	q := url.Values{}
	if search != "" {
		q.Set("search", search)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out []StaffCustomer
	err := c.get(ctx, request{
		path:     "/agent/staff/customers",
		host:     host,
		language: lang,
		query:    q,
		headers:  bearerHeaders(token),
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) StaffReviews(
	ctx context.Context, host, lang, token, status string, limit int,
) ([]StaffReview, error) {
	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out []StaffReview
	err := c.get(ctx, request{
		path:     "/agent/staff/reviews",
		host:     host,
		language: lang,
		query:    q,
		headers:  bearerHeaders(token),
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) StaffMessages(
	ctx context.Context, host, lang, token string, limit int,
) ([]StaffMessage, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out []StaffMessage
	err := c.get(ctx, request{
		path:     "/agent/staff/messages",
		host:     host,
		language: lang,
		query:    q,
		headers:  bearerHeaders(token),
		out:      &out,
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
