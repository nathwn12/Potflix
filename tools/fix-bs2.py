import io

p = r'<workspace-root>\PERSONAL\Potflix\src\main\java\me\potflix\rpc\Main.java'
lines = io.open(p, encoding='utf-8').read().split('\n')
BS = chr(92)
out = []
for ln in lines:
    if '[Group] prefix' in ln or '[CRC32] suffix' in ln:
        ln = ln.replace(BS + '[', BS * 2 + '[').replace(BS + ']', BS * 2 + ']')
    out.append(ln)
io.open(p, 'w', encoding='utf-8', newline='').write('\n'.join(out))
print('fixed2')
