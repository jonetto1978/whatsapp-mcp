"""request_history must speak the bridge's anchor vocabulary (review finding 2026-09-02)."""
import inspect
import re

import main


def _body_expr():
    src = inspect.getsource(main.request_history)
    return src


def test_direction_older_maps_to_anchor_oldest():
    src = _body_expr()
    assert '"older": "oldest"' in src, "direction=older must be sent as anchor=oldest"
    assert '"anchor": anchor' in src, "wire body must use the mapped anchor, not the raw direction"


def test_max_rounds_is_clamped():
    src = _body_expr()
    assert re.search(r"min\(int\(max_rounds\),\s*20\)", src), "max_rounds must be clamped"
