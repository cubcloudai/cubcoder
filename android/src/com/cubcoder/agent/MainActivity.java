package com.cubcoder.agent;

import android.app.Activity;
import android.content.SharedPreferences;
import android.os.Bundle;
import android.util.Log;
import android.view.View;
import android.widget.Button;
import android.widget.EditText;
import android.widget.ScrollView;
import android.widget.TextView;
import android.widget.Toast;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import gomobile.Agent;
import gomobile.Config;
import gomobile.Gomobile;
import gomobile.Callback;

public class MainActivity extends Activity implements Callback {

    private static final String TAG = "CubCoder";
    private static final String PREFS = "cubcoder_prefs";

    private SharedPreferences prefs;

    private EditText editMessage;
    private EditText editUrl;
    private EditText editApiKey;
    private EditText editModel;
    private TextView tvChat;
    private TextView tvStatus;
    private Button btnConnect;
    private Button btnSend;
    private Button btnReset;
    private ScrollView scrollView;

    private Agent agent;
    private volatile boolean connected = false;
    private volatile boolean agentRunning = false;

    private final ExecutorService executor = Executors.newSingleThreadExecutor();
    private final StringBuilder chatBuffer = new StringBuilder();

    @Override
    protected void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        setContentView(R.layout.activity_main);

        // Register global callback for gomobile callbacks
        Gomobile.registerCallback(this);

        editMessage = findViewById(R.id.edit_message);
        editUrl = findViewById(R.id.edit_url);
        editApiKey = findViewById(R.id.edit_api_key);
        editModel = findViewById(R.id.edit_model);
        tvChat = findViewById(R.id.tv_chat);
        tvStatus = findViewById(R.id.tv_status);
        btnConnect = findViewById(R.id.btn_connect);
        btnSend = findViewById(R.id.btn_send);
        btnReset = findViewById(R.id.btn_reset);
        scrollView = findViewById(R.id.scrollView);

        // Auto-scroll to the bottom whenever the chat content grows. Driven by a
        // layout-change listener so it fires *after* the TextView re-measures.
        // We scroll explicitly to the content height instead of using
        // fullScroll(FOCUS_DOWN), which is focus-based and misbehaves because
        // tv_chat is textIsSelectable (it steals focus).
        tvChat.addOnLayoutChangeListener((v, left, top, right, bottom,
                oldLeft, oldTop, oldRight, oldBottom) -> {
            if (bottom != oldBottom) {
                scrollView.post(() -> scrollView.scrollTo(0, tvChat.getBottom()));
            }
        });

        // Restore saved connection settings (persist across app sessions;
        // cleared only on uninstall/clear-data). Only override the layout
        // defaults when a value was actually saved.
        prefs = getSharedPreferences(PREFS, MODE_PRIVATE);
        String savedUrl = prefs.getString("base_url", null);
        if (savedUrl != null) editUrl.setText(savedUrl);
        String savedKey = prefs.getString("api_key", null);
        if (savedKey != null) editApiKey.setText(savedKey);
        String savedModel = prefs.getString("model", null);
        if (savedModel != null) editModel.setText(savedModel);

        // Toggle: connect when disconnected, disconnect when connected.
        btnConnect.setOnClickListener(v -> {
            if (connected) {
                disconnect();
            } else {
                connect();
            }
        });
        btnSend.setOnClickListener(v -> sendMessage());
        btnReset.setOnClickListener(v -> resetConversation());
    }

    private void connect() {
        String baseUrl = editUrl.getText().toString().trim();
        String apiKey = editApiKey.getText().toString().trim();
        String model = editModel.getText().toString().trim();

        if (baseUrl.isEmpty()) {
            Toast.makeText(this, "Enter base URL", Toast.LENGTH_SHORT).show();
            return;
        }

        // Persist settings so they survive app restarts.
        prefs.edit()
            .putString("base_url", baseUrl)
            .putString("api_key", apiKey)
            .putString("model", model)
            .apply();

        updateStatus("Connecting...");
        btnConnect.setEnabled(false);

        executor.execute(() -> {
            try {
                Config config = new Config();
                config.setBaseURL(baseUrl);
                config.setAPIKey(apiKey);
                config.setModel(model.isEmpty() ? "Qwen3.6-35B-A3B-FP8" : model);
                config.setAutoApprove(true);
                config.setMaxIters(150);

                agent = new Agent(config);

                runOnUiThread(() -> {
                    connected = true;
                    updateStatus("Connected");
                    btnConnect.setEnabled(true);
                    btnConnect.setText("Disconnect");
                    appendChatLine("\n✅ Connected to " + baseUrl + "\n");
                });
            } catch (Exception e) {
                Log.e(TAG, "Connect failed", e);
                runOnUiThread(() -> {
                    connected = false;
                    updateStatus("Failed: " + e.getMessage());
                    btnConnect.setEnabled(true);
                    btnConnect.setText("Connect");
                    Toast.makeText(MainActivity.this, "Connection failed: " + e.getMessage(), Toast.LENGTH_LONG).show();
                });
            }
        });
    }

    private void disconnect() {
        connected = false;
        final Agent a = agent;
        agent = null;
        btnConnect.setText("Connect");
        updateStatus("Disconnected");
        appendChatLine("\n🔌 Disconnected\n");
        // End the session (cancels any in-flight request) off the UI thread.
        if (a != null) {
            executor.execute(() -> {
                try {
                    a.end();
                } catch (Exception e) {
                    Log.e(TAG, "Disconnect failed", e);
                }
            });
        }
    }

    private void sendMessage() {
        String message = editMessage.getText().toString().trim();
        if (message.isEmpty() || !connected || agentRunning) {
            if (agentRunning) {
                Toast.makeText(this, "Agent is thinking...", Toast.LENGTH_SHORT).show();
            }
            return;
        }

        agentRunning = true;
        editMessage.setText("");
        btnSend.setEnabled(false);

        // Append user message
        appendChatLine("\n👤 " + message + "\n");

        executor.execute(() -> {
            try {
                agent.run(message);
            } catch (Exception e) {
                Log.e(TAG, "Run failed", e);
                appendChatLine("\n❌ Error: " + e.getMessage() + "\n");
            }

            runOnUiThread(() -> {
                agentRunning = false;
                btnSend.setEnabled(true);
                updateStatus("Ready");
            });
        });
    }

    private void resetConversation() {
        if (agent != null && connected) {
            executor.execute(() -> {
                try {
                    agent.reset();
                    runOnUiThread(() -> {
                        chatBuffer.setLength(0);
                        tvChat.setText("Conversation reset. Ask cubcoder to code...\n");
                        updateStatus("Reset");
                        Toast.makeText(MainActivity.this, "Conversation reset", Toast.LENGTH_SHORT).show();
                    });
                } catch (Exception e) {
                    Log.e(TAG, "Reset failed", e);
                }
            });
        }
    }

    // Callback implementation - gobind generates lowercase method names
    @Override
    public void onText(String delta) {
        appendChatLine(delta);
    }

    @Override
    public void onToolCall(String name, String args) {
        appendChatLine("\n⚙️ Tool: " + name + "\n");
    }

    @Override
    public void onToolResult(String name, String result) {
        String summary = result.length() > 80 ? result.substring(0, 80) + "..." : result;
        appendChatLine("  → " + name + ": " + summary + "\n");
    }

    @Override
    public void onDone(String text) {
        // Final answer text is already streamed through onText
    }

    @Override
    public void onError(String err) {
        appendChatLine("\n❌ Error: " + err + "\n");
    }

    @Override
    public void onStatus(String text) {
        updateStatus(text);
    }

    private void appendChatLine(String text) {
        runOnUiThread(() -> {
            chatBuffer.append(text);
            tvChat.setText(chatBuffer.toString());
            // Scrolling is handled by the OnLayoutChangeListener on tvChat, which
            // fires once the new text has been measured.
        });
    }

    private void updateStatus(String text) {
        runOnUiThread(() -> tvStatus.setText(text));
    }

    @Override
    protected void onDestroy() {
        super.onDestroy();
        if (agent != null) {
            agent.end();
        }
        executor.shutdown();
    }
}
