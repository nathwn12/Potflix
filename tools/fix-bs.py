import io

p = r'<workspace-root>\PERSONAL\Potflix\src\main\java\me\potflix\rpc\Main.java'
lines = io.open(p, encoding='utf-8').read().split('\n')
out = []
for ln in lines:
    if 'FANSUB = ' in ln or 'NCBOOK = ' in ln or '[Group] prefix' in ln or '[CRC32] suffix' in ln or 'nhead = ' in ln:
        # double the backslash before s and . but NOT before u (unicode escape)
        tmp = ln.replace(chr(92) + 's', chr(92) * 2 + 's').replace(chr(92) + '.', chr(92) * 2 + '.')
        # oops guard: lines already containing double backslash would quadruple;
        # our new lines only had singles, existing lines aren't in this set. verify:
        out.append(tmp)
    else:
        out.append(ln)
io.open(p, 'w', encoding='utf-8', newline='').write('\n'.join(out))
print('fixed')
