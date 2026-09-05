// Frozen user-visible strings (SPEC §9: TG texts are contract surface).
package menu

import ()

const (
	welcomeText            = "Hi! I'm the tgNtfy gate. Your service events arrive in YOUR personal Telegram forum group — one topic per linked service.\n1) Link a service: /link\n2) Create your group: /setup\nManage anything in /menu."
	helpText               = "Commands:\n/link — link a service\n/setup — create your forum group\n/connect <code> — bind this group (send in your group)\n/menu — manage services & types\n/status — delivery status\n/undelivered — failed deliveries"
	setupStep1Text         = "📋 STEP 1/2 — Create a new **private** group in Telegram (any name, e.g. 'my tgntfy'). Members: only you. Then add **me** as **Administrator** with the permission **Manage topics** (group → Administrators → Edit → Manage topics ✓).\n\nWhen done, tap ✅ I did it."
	setupErrNoGroup        = "I can't find your group yet. Open your private group and send any message there (e.g. /setup), then tap ✅ I did it again."
	setupErrNoForum        = "This group doesn't have **Topics** enabled — create a forum-style group (group settings → Topics → on) or a new one."
	setupErrNoAdminRight   = "I can see the group, but I'm missing the **Manage topics** admin right. Grant it (group → Administrators → Edit), then tap ✅ I did it again."
	setupErrSenderNotAdmin = "Only an **admin of the group** can finish setup."
)
