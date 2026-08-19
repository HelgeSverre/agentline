package setup

import "github.com/HelgeSverre/agentline/integrations"

// sharedSkill is the Agent Skill installed for every harness target. The
// canonical source lives in integrations/skills so setup and the repository
// cannot drift apart.
var sharedSkill = integrations.Skill

var MCPTools = []string{"create_room", "join_room", "send_message", "read_messages", "wait_for_message", "end_conversation", "get_room_status"}
