"""Unit tests for the production frontend bundling path.

Verifies the in-memory zip the SDK streams to Core: entry names are
slash-separated and relative to the dist dir, and the returned SHA-256 matches
the bytes so Core can verify the upload. Does not require a running Core.

Run: pytest sdk/python/
"""

import hashlib
import io
import os
import sys
import zipfile

sys.path.insert(0, os.path.abspath(os.path.join(os.path.dirname(__file__), "..")))

import pytest  # noqa: E402

from sdk.bridge import _build_frontend_zip  # noqa: E402


def test_build_frontend_zip(tmp_path):
    (tmp_path / "index.html").write_text("<html>hi</html>")
    assets = tmp_path / "assets"
    assets.mkdir()
    (assets / "app.js").write_text("console.log(1)")

    data, sha = _build_frontend_zip(str(tmp_path))

    assert sha == hashlib.sha256(data).hexdigest()

    with zipfile.ZipFile(io.BytesIO(data)) as zf:
        names = set(zf.namelist())
        assert names == {"index.html", "assets/app.js"}
        assert zf.read("index.html") == b"<html>hi</html>"
        assert zf.read("assets/app.js") == b"console.log(1)"


def test_build_frontend_zip_missing_dir(tmp_path):
    with pytest.raises(NotADirectoryError):
        _build_frontend_zip(str(tmp_path / "nope"))
