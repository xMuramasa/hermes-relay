# Rewinds a Hermes conversation to just before one of its user messages, with Hermes' own SessionDB (run by
# trismegistus-agents with Hermes' interpreter after hermes_bootstrap). argv: session id, message id. Prints
# {"rewound", "turns"}; exits 3 when the id is not one of the session's user messages. Never prints message text.
import json
import sys

from hermes_state import SessionDB

sid, mid = sys.argv[1], int(sys.argv[2])
db = SessionDB()
conv = db.get_messages_as_conversation(sid, include_row_ids=True, repair_alternation=True)
users = [m for m in conv if m.get("role") == "user"]
ordinal = next((i for i, m in enumerate(users) if m.get("_row_id") == mid), None)
if ordinal is None:
    sys.exit(3)
out = db.rewind_user_turn(sid, ordinal)
print(json.dumps({"rewound": out.rewound_count, "turns": out.turns_undone}))
