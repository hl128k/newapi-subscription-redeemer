#!/usr/bin/env python3
"""Compatibility wrapper for the src-layout service package."""

from __future__ import annotations

import sys
from pathlib import Path

ROOT_DIR = Path(__file__).resolve().parent
SRC_DIR = ROOT_DIR / "src"
if str(SRC_DIR) not in sys.path:
    sys.path.insert(0, str(SRC_DIR))

from newapi_subscription_redeemer.redeemer import *  # noqa: F401,F403,E402
from newapi_subscription_redeemer.redeemer import main  # noqa: E402


if __name__ == "__main__":
    raise SystemExit(main())
