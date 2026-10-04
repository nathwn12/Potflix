import io

p = r'<workspace-root>\PERSONAL\Potflix\src\main\java\me\potflix\rpc\Main.java'
t = io.open(p, encoding='utf-8').read()
old = 'context + " ' + chr(20) + ' " + at'
assert old in t, 'pattern missing'
t = t.replace(old, 'context + " - " + at')
io.open(p, 'w', encoding='utf-8', newline='').write(t)
print('cleaned')
