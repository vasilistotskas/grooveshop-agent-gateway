"""Prepares the store for the real-stack tests and prints what they need.

Runs inside `manage.py shell`, against a throwaway database. The last output
line is `REALSTACK <json>`, because Django's logging shares stdout.
"""

import json
import secrets
from datetime import timedelta

from allauth.account.models import EmailAddress
from allauth.idp.oidc.models import Client, Token
from django.contrib.auth import get_user_model
from django.utils import timezone
from django_tenants.utils import schema_context

from pay_way.models import PayWay
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
# only while the store holds the provider's credentials. These are not real:
# the stack leads the provider's hosts nowhere (paymentHosts).
store.viva_wallet_client_id = "realstack"
store.viva_wallet_client_secret = "realstack"
store.save(update_fields=["viva_wallet_client_id", "viva_wallet_client_secret"])

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
