package auction

import "testing"

func testPaymentOrder() Order {
	return Order{
		ID:              "order_1",
		MainAccountID:   "main_1",
		LotID:           "lot_1",
		RoomID:          "room_1",
		LotTitle:        "test lot",
		BuyerUserID:     "buyer_1",
		BuyerNickname:   "buyer",
		Status:          OrderStatusPendingPayment,
		PaymentStatus:   PaymentStatusInit,
		Amount:          10000,
		Currency:        "CNY",
		CreatedAtUnixMs: 100,
		UpdatedAtUnixMs: 100,
		ExpiresAtUnixMs: 200000,
		Version:         1,
	}
}

func TestPaymentReplayRejectsDifferentRequestFingerprint(t *testing.T) {
	order := testPaymentOrder()
	payment, err := NewPayment("pay_1", order, "mock", "idem_1", order.Amount, order.Currency, 101)
	if err != nil {
		t.Fatalf("NewPayment() error = %v", err)
	}

	if err := ValidatePaymentReplay(*payment, order, order.Amount+1, order.Currency, "mock"); err == nil {
		t.Fatal("ValidatePaymentReplay() expected fingerprint mismatch error")
	}
	if err := ValidatePaymentReplay(*payment, order, order.Amount, order.Currency, "mock"); err != nil {
		t.Fatalf("ValidatePaymentReplay() same request error = %v", err)
	}
}

func TestPaymentSuccessRequiresProviderPaymentID(t *testing.T) {
	order := testPaymentOrder()
	payment, err := NewPayment("pay_1", order, "mock", "idem_1", order.Amount, order.Currency, 101)
	if err != nil {
		t.Fatalf("NewPayment() error = %v", err)
	}
	if err := payment.MarkProcessing(102); err != nil {
		t.Fatalf("MarkProcessing() error = %v", err)
	}
	if err := payment.MarkSuccess("", 103); err == nil {
		t.Fatal("MarkSuccess() expected provider payment id error")
	}
	if err := payment.MarkSuccess("mock_provider_pay_1", 104); err != nil {
		t.Fatalf("MarkSuccess() error = %v", err)
	}
	if payment.ProviderPaymentID != "mock_provider_pay_1" {
		t.Fatalf("ProviderPaymentID = %q, want mock_provider_pay_1", payment.ProviderPaymentID)
	}
}

func TestReconcileSuccessfulPaymentMarksPendingOrderPaid(t *testing.T) {
	order := testPaymentOrder()
	payment, err := NewPayment("pay_1", order, "mock", "idem_1", order.Amount, order.Currency, 101)
	if err != nil {
		t.Fatalf("NewPayment() error = %v", err)
	}
	if err := payment.MarkProcessing(102); err != nil {
		t.Fatalf("MarkProcessing() error = %v", err)
	}
	if err := payment.MarkSuccess("mock_provider_pay_1", 103); err != nil {
		t.Fatalf("MarkSuccess() error = %v", err)
	}

	changed, err := ReconcileOrderPayment(&order, *payment, 200)
	if err != nil {
		t.Fatalf("ReconcileOrderPayment() error = %v", err)
	}
	if !changed {
		t.Fatal("ReconcileOrderPayment() changed = false, want true")
	}
	if order.Status != OrderStatusPaid || order.PaymentStatus != PaymentStatusSuccess {
		t.Fatalf("order status = %s/%s, want PAID/SUCCESS", order.Status, order.PaymentStatus)
	}
	if order.PaymentID != payment.ID || order.PaymentProvider != "mock" || order.PaymentProviderPaymentID != "mock_provider_pay_1" {
		t.Fatalf("order payment metadata not copied: %+v", order)
	}
	if order.PaymentReconciledAtUnixMs != 200 {
		t.Fatalf("PaymentReconciledAtUnixMs = %d, want 200", order.PaymentReconciledAtUnixMs)
	}
}
