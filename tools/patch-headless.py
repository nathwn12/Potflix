import io

p = r'<workspace-root>\PERSONAL\Potflix\src\main\java\me\potflix\rpc\Main.java'
t = io.open(p, encoding='utf-8').read()
n0 = len(t)

# 1. drop swing imports (tray uses java.awt only)
old = 'import javax.swing.*;\nimport javax.swing.border.EmptyBorder;\n'
assert old in t, 'swing imports not found'
t = t.replace(old, '', 1)

# 2. drop UI state fields
BS = chr(92)
anchor = '    static volatile String uiTime = "";'
assert anchor in t, 'ui fields not found'
start = t.index('    static final Color BG')
end = t.index('    static JProgressBar progress;') + len('    static JProgressBar progress;')
t = t[:start] + '    // headless: no UI state kept.\n' + t[end:]

# 3. main(): no window
old = '        buildTray();\n        buildWindow();\n'
assert old in t, 'main hook not found'
t = t.replace(old, '        buildTray();\n', 1)

# 4. tray: Exit only
old = ('        MenuItem open = new MenuItem("Open Potflix");\n'
'        open.addActionListener(e -> showWindow());\n'
'        MenuItem exit = new MenuItem("Exit Potflix");\n'
'        exit.addActionListener(e -> System.exit(0));\n'
'        popup.add(open);\n'
'        popup.addSeparator();\n'
'        popup.add(exit);\n')
assert old in t, 'tray menu not found'
t = t.replace(old,
'        MenuItem exit = new MenuItem("Exit Potflix");\n'
'        exit.addActionListener(e -> System.exit(0));\n'
'        popup.add(exit);\n', 1)
old = '        icon.addActionListener(e -> showWindow());\n'
assert old in t, 'tray click not found'
t = t.replace(old, '', 1)

# 5. cut window methods (frame field -> EOF), keep no-op setUi sink
idx = t.index('\n    static JFrame frame;')
t = (t[:idx]
+ '\n    // headless: status sink so polling code stays untouched (JITs away).\n'
+ '    static void setUi(String title, String status, String time, int cur, int max) { }\n'
+ '}\n')

# 6. idle poll slower when PotPlayer absent
old = '            Thread.sleep(1600);\n'
assert old in t, 'idle sleep not found'
t = t.replace(old, '            Thread.sleep(5000);\n', 1)

# 7. Cleaned gains episodeName; populate it in clean()
old = ('    static class Cleaned {\n'
'        String title = "";\n'
'        String display = "";\n'
'        String episodeTag = "";\n'
'    }\n')
assert old in t, 'Cleaned not found'
t = t.replace(old,
'    static class Cleaned {\n'
'        String title = "";\n'
'        String display = "";\n'
'        String episodeTag = "";\n'
'        String episodeName = "";\n'
'    }\n', 1)
old = '            if (!epName.contains(" ")) epName = "";\n'
assert old in t, 'epName guard not found'
t = t.replace(old, old + '            out.episodeName = epName;\n', 1)

io.open(p, 'w', encoding='utf-8', newline='').write(t)
print('headless patch ok, bytes %d -> %d' % (n0, len(t)))
