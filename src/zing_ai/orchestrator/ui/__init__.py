"""Rich-based inline UI for the Zing orchestrator."""

from rich.console import Console

console = Console(stderr=True)

# Public functions from submodules will be re-exported here
# as they are created in subsequent steps:
#   from .menus import ...
#   from .progress import ...
