"""Per-request authenticated user context, propagated via contextvars."""

from contextvars import ContextVar
from typing import List, Optional

from pydantic import BaseModel


class UserContext(BaseModel):
    user_id: str
    roles: List[str] = []
    permissions: List[str] = []


_user_context: ContextVar[Optional[UserContext]] = ContextVar(
    "user_context", default=None
)


def get_user() -> Optional[UserContext]:
    """Return the user injected by Core for the current request, if any."""
    return _user_context.get()
