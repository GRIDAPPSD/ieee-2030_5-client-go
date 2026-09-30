"""GridLAB-D fleet sidecar: one process, one model, driven over a Unix socket."""

from .protocol import PROTOCOL_VERSION

__all__ = ["PROTOCOL_VERSION"]
