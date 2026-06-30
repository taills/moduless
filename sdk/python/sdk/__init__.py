"""Multi-language framework Python SDK.

Bridges Core's reverse gRPC HTTP tunnel into any ASGI application (FastAPI)
without the extension opening a listening port.
"""

from .bridge import start_sdk, Config
from .context import get_user, UserContext

__all__ = ["start_sdk", "Config", "get_user", "UserContext"]
