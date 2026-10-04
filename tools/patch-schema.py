import io, sys

p = r'<workspace-root>\PERSONAL\Potflix\src\main\java\me\potflix\rpc\Main.java'
t = io.open(p, encoding='utf-8').read()

BS = chr(92)  # backslash
U = BS + 'u2022'

old_playing = (
'            if (ps == PlayStatus.Running) {\n'
'                activity.timestamps().setStart(Instant.now().minusMillis(ct));\n'
'                activity.setState(trunc("Playing" + (cleaned.episodeTag.isEmpty() ? "" : " ' + U + ' " + cleaned.episodeTag)\n'
'                        + " ' + U + ' " + fmt(ct) + " / " + fmt(total), 120));\n'
'                activity.assets().setSmallImage("play");\n'
'                activity.assets().setSmallText("");\n'
'            } else if (ps == PlayStatus.Paused) {\n'
'                activity.setState(trunc("Paused" + (cleaned.episodeTag.isEmpty() ? "" : " ' + U + ' " + cleaned.episodeTag)\n'
'                        + " ' + U + ' " + fmt(ct) + " / " + fmt(total), 120));\n'
'                activity.assets().setSmallImage("pause");\n'
'                activity.assets().setSmallText("");\n'
'            } else {\n'
'                activity.setState("Stopped");\n'
'                activity.assets().setSmallImage("stop");\n'
'                activity.assets().setSmallText("");\n'
'            }\n'
)

new_block = (
'            // Netflix-native: text carries title+episode only; elapsed+remaining\n'
'            // come from Discord native timers (start+end), never duplicated.\n'
'            if (ps == PlayStatus.Running) {\n'
'                Instant start = Instant.now().minusMillis(ct);\n'
'                activity.timestamps().setStart(start);\n'
'                if (total > 0) activity.timestamps().setEnd(start.plusMillis(total));\n'
'                activity.assets().setSmallImage("play");\n'
'                activity.assets().setSmallText("");\n'
'            } else if (ps == PlayStatus.Paused) {\n'
'                activity.assets().setSmallImage("pause");\n'
'                activity.assets().setSmallText("");\n'
'            } else {\n'
'                activity.assets().setSmallImage("stop");\n'
'                activity.assets().setSmallText("");\n'
'            }\n'
)

assert old_playing in t, 'PATTERN NOT FOUND'
t = t.replace(old_playing, new_block)

old_state = '            activity.setDetails(trunc(cleaned.title, 120));\n'
new_state = (
'            activity.setDetails(trunc(cleaned.title, 120));\n'
'            String context = "";\n'
'            if (!cleaned.episodeTag.isEmpty()) {\n'
'                context = cleaned.episodeTag\n'
'                        + (cleaned.episodeName.isEmpty() ? "" : " \\"" + trunc(cleaned.episodeName, 60) + "\\"");\n'
'            }\n'
'            activity.setState(trunc(context, 120));\n'
)
assert old_state in t, 'DETAILS PATTERN NOT FOUND'
t = t.replace(old_state, new_state, 1)

io.open(p, 'w', encoding='utf-8', newline='').write(t)
print('patched ok')
