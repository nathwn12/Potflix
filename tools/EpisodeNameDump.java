import java.lang.reflect.Field;
import java.lang.reflect.Method;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Paths;

// Scratch evidence harness: reflectively dump clean() without starting the app.
public class EpisodeNameDump {
    private static String json(String value) {
        StringBuilder out = new StringBuilder("\"");
        for (int i = 0; i < value.length(); i++) {
            char c = value.charAt(i);
            if (c == '"' || c == '\\') out.append('\\').append(c);
            else if (c < 32) out.append(String.format("\\u%04x", (int) c));
            else out.append(c);
        }
        return out.append('"').toString();
    }

    public static void main(String[] args) throws Exception {
        Class<?> main = Class.forName("me.potflix.rpc.Main");
        Method clean = main.getDeclaredMethod("clean", String.class);
        clean.setAccessible(true);
        Class<?> cleaned = Class.forName("me.potflix.rpc.Main$Cleaned");
        String[] names = {"title", "episodeTag", "episodeName", "display"};
        Field[] fields = new Field[names.length];
        for (int i = 0; i < names.length; i++) {
            fields[i] = cleaned.getDeclaredField(names[i]);
            fields[i].setAccessible(true);
        }
        for (String raw : Files.readAllLines(Paths.get(args[0]), StandardCharsets.UTF_8)) {
            if (raw.trim().isEmpty()) continue;
            String input = raw.endsWith(" - PotPlayer")
                    ? raw.substring(0, raw.length() - " - PotPlayer".length()) : raw;
            Object result = clean.invoke(null, input);
            StringBuilder line = new StringBuilder("{\"raw\":").append(json(raw));
            for (int i = 0; i < names.length; i++) {
                line.append(',').append(json(names[i])).append(':')
                        .append(json((String) fields[i].get(result)));
            }
            System.out.println(line.append('}'));
        }
    }
}
