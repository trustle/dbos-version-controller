"""One log line per event, with ``key=value`` pairs after the message.

The controller emits structured events but must not carry a logging framework
into an operator image. This helper writes each event through the standard
library, so an operator can parse the pairs and a library user can configure
``logging`` in the usual way.
"""

import logging
from typing import Any


def _render(event: str, fields: dict[str, Any]) -> str:
    if not fields:
        return event
    pairs = " ".join(f"{key}={value!r}" for key, value in fields.items())
    return f"{event} {pairs}"


class EventLogger:
    """A thin wrapper turning keyword fields into ``key=value`` text."""

    def __init__(self, name: str) -> None:
        self._logger = logging.getLogger(name)

    def _log(self, level: int, event: str, fields: dict[str, Any]) -> None:
        exc_info = fields.pop("exc_info", False)
        self._logger.log(level, _render(event, fields), exc_info=exc_info)

    def debug(self, event: str, **fields: Any) -> None:
        self._log(logging.DEBUG, event, fields)

    def info(self, event: str, **fields: Any) -> None:
        self._log(logging.INFO, event, fields)

    def warning(self, event: str, **fields: Any) -> None:
        self._log(logging.WARNING, event, fields)

    def error(self, event: str, **fields: Any) -> None:
        self._log(logging.ERROR, event, fields)

    def exception(self, event: str, **fields: Any) -> None:
        fields["exc_info"] = True
        self._log(logging.ERROR, event, fields)


def get_logger(name: str) -> EventLogger:
    return EventLogger(name)


def configure_logging(level: str) -> None:
    """Configure the root logger once, for the ``python -m`` entry point."""
    logging.basicConfig(
        level=level.upper(),
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
