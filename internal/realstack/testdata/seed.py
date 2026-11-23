"""Prepares the store for the real-stack tests and prints what they need.

Runs inside `manage.py shell`, against a throwaway database. The last output
line is `REALSTACK <json>`, because Django's logging shares stdout.
"""

import json
import os
import secrets
from datetime import timedelta
from decimal import Decimal

from allauth.account.models import EmailAddress
from allauth.idp.oidc.models import Client, Token
from django.conf import settings
from django.contrib.auth import get_user_model
from django.utils import timezone
from django_tenants.utils import schema_context
from djmoney.money import Money

from core.celery import celery_app
from pay_way.models import PayWay
from shipping.models.provider import ShippingProvider
from shipping.models.rate import ShippingRate
from shipping_acs.models import AcsStation
from shipping_boxnow.models import BoxNowLocker
from tenant.models import Tenant

SCOPES = ["profile", "orders:read", "loyalty:read", "favourites:read"]


def mint(user, client, resources):
    raw = secrets.token_urlsafe(32)
    token = Token(
        type=Token.Type.ACCESS_TOKEN,
        client=client,
        user=user,
        expires_at=timezone.now() + timedelta(hours=2),
    )
    token.set_value(raw)
    token.set_scopes(SCOPES)
    token.set_resources(resources)
    token.save()
    return raw


store = Tenant.objects.get(schema_name="demostore")
primary = store.get_primary_domain().domain

# The store offers its online pay way, so a test can place an order that
# awaits its payment. Django lists that pay way, and takes an order with it,
# only while the store holds all four of the provider's credentials (the
# OAuth pair and the Basic pair, VivaWalletPaymentProvider
# .is_configured_for_tenant). These are not real: the stack leads the
# provider's hosts nowhere (paymentHosts).
viva = {
    "viva_wallet_merchant_id": "realstack",
    "viva_wallet_api_key": "realstack",
    "viva_wallet_client_id": "realstack",
    "viva_wallet_client_secret": "realstack",
}
for field, value in viva.items():
    setattr(store, field, value)
store.save(update_fields=list(viva))

# The carriers' placeholder credentials, so the store offers pickup at
# ACS points and BOX NOW lockers (each carrier hides itself without its
# credentials). The owner allows these ONLY here, on this throwaway
# real-stack tenant: never on a local database, staging or production.
# They fail closed: they are not real, every carrier URL Django reads
# must lead nowhere (the stack's carrierDead) and no Celery worker may be
# running, or nothing is seeded.
dead = os.environ["REALSTACK_CARRIER_DEAD_URL"]
carrier_urls = {
    name: getattr(settings, name)
    for name in (
        "ACS_API_BASE_URL",
        "BOXNOW_STAGE_API_BASE_URL",
        "BOXNOW_STAGE_LOCATION_API_BASE_URL",
        "BOXNOW_PRODUCTION_API_BASE_URL",
        "BOXNOW_PRODUCTION_LOCATION_API_BASE_URL",
    )
}
if any(url != dead for url in carrier_urls.values()):
    raise RuntimeError(f"a carrier URL leads somewhere: {carrier_urls}")
if settings.CELERY_TASK_ALWAYS_EAGER:
    raise RuntimeError("Celery runs tasks inline: a carrier task would run")
if celery_app.control.ping(timeout=2):
    raise RuntimeError("a Celery worker is running: a carrier task would run")
placeholders = {
    "acs_api_key": "realstack-not-a-key",
    "acs_company_id": "realstack-not-a-company",
    "acs_company_password": "realstack-not-a-password",
    "acs_user_id": "realstack-not-a-user",
    "acs_user_password": "realstack-not-a-password",
    "box_now_client_id": "realstack-not-a-client",
    "box_now_client_secret": "realstack-not-a-secret",
    "box_now_partner_id": "realstack-not-a-partner",
    "box_now_warehouse_id": "realstack-not-a-warehouse",
    "box_now_notify_phone": "+300000000000",
    "box_now_webhook_secret": "realstack-not-a-secret",
}
for field, value in placeholders.items():
    setattr(store, field, value)
store.box_now_live_mode = False
store.save(update_fields=[*placeholders, "box_now_live_mode"])

with schema_context("demostore"):
    # One ACS Smartpoint and one BOX NOW locker in GR, the points the
    # pickup tests order to, and the two carriers' pickup rates there.
    for code in ("acs", "boxnow"):
        provider = ShippingProvider.objects.get(code=code)
        provider.is_active = True
        provider.save(update_fields=["is_active"])
        ShippingRate.objects.update_or_create(
            provider=provider,
            country_id="GR",
            kind="pickup_point",
            defaults={"price": Money(Decimal("2.50"), "EUR"), "is_active": True},
        )
    now = timezone.now()
    AcsStation.objects.update_or_create(
        external_id="RST",
        branch_code="1",
        defaults={
            "shop_kind": 7,
            "name": "Real-stack Smartpoint",
            "address_line_1": "Πανεπιστημίου 30",
            "city": "Αθήνα",
            "postal_code": "10679",
            "country_code": "GR",
            "is_active": True,
            "last_synced_at": now,
        },
    )
    BoxNowLocker.objects.update_or_create(
        external_id="9001",
        defaults={
            "name": "Real-stack locker",
            "address_line_1": "Σταδίου 10",
            "address_line_2": "Αθήνα",
            "postal_code": "10564",
            "country_code": "GR",
            "lat": Decimal("37.9780000"),
            "lng": Decimal("23.7330000"),
            "is_active": True,
            "last_synced_at": now,
        },
    )

with schema_context("demostore"):
    online = PayWay.objects.get(provider_code="viva_wallet")
    online.active = True
    online.save(update_fields=["active"])

    user, _ = get_user_model().objects.get_or_create(email="buyer@realstack.test")
    password = secrets.token_urlsafe(24)
    user.set_password(password)
    user.is_active = True
    user.save()
    EmailAddress.objects.update_or_create(
        user=user,
        email=user.email,
        defaults={"verified": True, "primary": True},
    )
    client = Client.objects.create(name="Real-stack test client")
    print(
        "REALSTACK "
        + json.dumps(
            {
                "acpToken": store.acp_bearer_token,
                "buyerEmail": user.email,
                "buyerPassword": password,
                "linkedToken": mint(user, client, [f"https://{primary}/mcp"]),
                "foreignToken": mint(user, client, ["https://other.example/mcp"]),
            }
        )
    )
