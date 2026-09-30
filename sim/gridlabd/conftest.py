"""Makes gldsidecar and models importable from tests regardless of the
invoking working directory, without installing the package."""

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
