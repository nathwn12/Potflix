package me.potflix.rpc;

import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Paths;

/** Independent, offline verifier: never calls Main.main or native Discord/Player APIs. */
public final class EpisodeVerifier2 {
    private static String json(String value) {
        StringBuilder out = new StringBuilder("\"");
        for (char ch : value.toCharArray()) {
            switch (ch) {
                case '\\': out.append("\\\\"); break;
                case '"': out.append("\\\""); break;
                case '\n': out.append("\\n"); break;
                case '\r': out.append("\\r"); break;
                case '\t': out.append("\\t"); break;
                default:
                    if (ch < 32) out.append(String.format("\\u%04x", (int) ch));
                    else out.append(ch);
            }
        }
        return out.append('"').toString();
    }

    public static void main(String[] args) throws Exception {
        boolean newStateRule = Boolean.parseBoolean(args[1]);
        int row = 0;
        for (String raw : Files.readAllLines(Paths.get(args[0]), StandardCharsets.UTF_8)) {
            Main.Cleaned cleaned = Main.clean(raw);
            // Same field construction and order of truncations as pushActivity.
            String details = Main.trunc(cleaned.title, 120);
            String context = "";
            if (!cleaned.episodeTag.isEmpty()) {
                context = cleaned.episodeTag
                        + (cleaned.episodeName.isEmpty() ? "" : " \"" + Main.trunc(cleaned.episodeName, 60) + "\"");
            }
            context = Main.trunc(context, 120);
            String state = newStateRule && context.equals(details) ? "" : context;
            System.out.println("{\"row\":" + (++row) + ",\"raw\":" + json(raw)
                    + ",\"title\":" + json(cleaned.title)
                    + ",\"episodeTag\":" + json(cleaned.episodeTag)
                    + ",\"episodeName\":" + json(cleaned.episodeName)
                    + ",\"display\":" + json(cleaned.display)
                    + ",\"details\":" + json(details)
                    + ",\"state\":" + json(state) + "}");
        }
    }
}
