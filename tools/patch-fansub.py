import io

p = r'<workspace-root>\PERSONAL\Potflix\src\main\java\me\potflix\rpc\Main.java'
t = io.open(p, encoding='utf-8').read()
n0 = len(t)

BS = chr(92)  # one backslash
J = BS + BS   # how a regex backslash appears in the java source

def rep(old, new, tag):
    global t
    assert old in t, tag + ' NOT FOUND'
    t = t.replace(old, new, 1)

# 1. new patterns after ALT_EP
old = '    static final Pattern ALT_EP = Pattern.compile("(?i)^(.+?)[. _' + J + '-]+(?:EP?|#)?[ . _' + J + '-]*(' + J + 'd{1,3})(?:[. _' + J + '-]+(.*))?$");\n'
rep(old, old
+ '    static final Pattern FANSUB = Pattern.compile("(?i)^(.+?)' + BS + 's*[-;]' + BS + 's*(?:Ep?' + BS + '.?' + BS + 's*)?(' + J + 'd{1,4})(?:' + BS + 's*[-;]' + BS + 's*(.*))?$");\n'
+ '    static final Pattern NCBOOK = Pattern.compile("(?i)^(.+?)[. _' + J + '-]+NC(OP|ED)(' + J + 'd{1,3})' + BS + 's*$");\n', 'patterns')

# 2. strip [Group] prefix + [CRC32] suffix + NCOP/NCED branch after ext strip
old = '        String s = EXT.matcher(raw.trim()).replaceAll("");\n'
rep(old, old
+ '        s = s.replaceAll("^' + BS + 's*' + BS + '[[^' + BS + ']]+]' + BS + 's*", ""); // [Group] prefix\n'
+ '        s = s.replaceAll("' + BS + 's*' + BS + '[[A-Fa-f0-9]{8}]' + BS + 's*$", ""); // [CRC32] suffix\n'
+ '        Matcher nc = NCBOOK.matcher(s);\n'
+ '        if (nc.matches()) {\n'
+ '            Cleaned ncout = new Cleaned();\n'
+ '            String nhead = nc.group(1).replaceAll("(?i)[. _' + J + '-]+S' + J + 'd{1,2}$", "");\n'
+ '            ncout.title = trunc(stripTags(nhead), 100);\n'
+ '            ncout.episodeTag = ("OP".equalsIgnoreCase(nc.group(2)) ? "OP " : "ED ") + Integer.parseInt(nc.group(3));\n'
+ '            ncout.display = ncout.title + " ' + BS + 'u2014 " + ncout.episodeTag;\n'
+ '            ncout.episodeName = "";\n'
+ '            return ncout;\n'
+ '        }\n', 'ncbranch')

# 3. replace whole ALT_EP else-block with fansub-first version
old = ('        } else {\n'
'            Matcher a = ALT_EP.matcher(s);\n'
'            // only treat as episode number when the head looks like a show name + short tail\n'
'            if (a.matches() && a.group(1).length() >= 3 && a.group(2).length() <= 3 && a.group(3) != null && a.group(3).length() < 40\n'
'                    && s.matches("(?i).*[. _' + J + '-](EP?|#)?[ . _' + J + '-]*' + J + 'd{1,3}([. _' + J + '-].*)?$") && stripTags(s).length() > a.group(1).length()) {\n'
'                seriesPart = a.group(1);\n'
'                epTag = "E" + Integer.parseInt(a.group(2));\n'
'                rest = a.group(3);\n'
'            }\n'
'        }\n')
new = ('        } else {\n'
'            Matcher f = FANSUB.matcher(s);\n'
'            boolean took = false;\n'
'            if (f.matches()) {\n'
'                int fnum = Integer.parseInt(f.group(2));\n'
'                boolean yearLike = f.group(2).length() == 4 && fnum >= 1900 && fnum <= 2100;\n'
'                if (!yearLike) {\n'
'                    seriesPart = f.group(1);\n'
'                    epTag = "E" + fnum;\n'
'                    rest = f.group(3);\n'
'                    took = true;\n'
'                }\n'
'            }\n'
'            if (!took) {\n'
'                Matcher a = ALT_EP.matcher(s);\n'
'                // only treat as episode number when the head looks like a show name + short tail\n'
'                if (a.matches() && a.group(1).length() >= 3 && a.group(2).length() <= 3 && a.group(3) != null && a.group(3).length() < 40\n'
'                        && s.matches("(?i).*[. _' + J + '-](EP?|#)?[ . _' + J + '-]*' + J + 'd{1,3}([. _' + J + '-].*)?$") && stripTags(s).length() > a.group(1).length()) {\n'
'                    seriesPart = a.group(1);\n'
'                    epTag = "E" + Integer.parseInt(a.group(2));\n'
'                    rest = a.group(3);\n'
'                }\n'
'            }\n'
'        }\n')
rep(old, new, 'fansub')

io.open(p, 'w', encoding='utf-8', newline='').write(t)
print('fansub patch ok, bytes %d -> %d' % (n0, len(t)))
