/**
 * Search Provider Abstraction for Speax Search Tool
 */

export class SearchProvider {
    constructor(name) {
        this.name = name;
    }

    /**
     * Perform a search query.
     * @param {string} query - The search query string.
     * @returns {Promise<Array>} - A promise that resolves to an array of results.
     */
    async search(query) {
        throw new Error("search() must be implemented by subclass");
    }
}

/**
 * DuckDuckGo Instant Answer API Provider (Free, No Key)
 * Note: This mainly provides "instant answers" and snippets, not full web results.
 */
export class DuckDuckGoProvider extends SearchProvider {
    constructor() {
        super("DuckDuckGo");
    }

    async search(query) {
        const url = `https://api.duckduckgo.com/?q=${encodeURIComponent(query)}&format=json&no_html=1&skip_disambig=1`;
        const response = await fetch(url);
        const data = await response.json();

        const results = [];

        // Add Abstract if available
        if (data.AbstractText) {
            results.push({
                title: data.Heading || "Abstract",
                snippet: data.AbstractText,
                url: data.AbstractURL,
                source: "DuckDuckGo (Abstract)"
            });
        }

        // Add Related Topics
        if (data.RelatedTopics) {
            data.RelatedTopics.forEach(topic => {
                if (topic.Text && topic.FirstURL) {
                    results.push({
                        title: topic.Text.split(' - ')[0] || "Related Topic",
                        snippet: topic.Text,
                        url: topic.FirstURL,
                        source: "DuckDuckGo"
                    });
                } else if (topic.Topics) {
                    topic.Topics.forEach(subTopic => {
                        results.push({
                            title: subTopic.Text.split(' - ')[0] || "Related Topic",
                            snippet: subTopic.Text,
                            url: subTopic.FirstURL,
                            source: "DuckDuckGo"
                        });
                    });
                }
            });
        }

        return results.slice(0, 10);
    }
}

/**
 * SearXNG Provider (Uses public instances)
 * Provides full web search results.
 */
export class SearXNGProvider extends SearchProvider {
    constructor(instanceUrl = "https://searx.be") {
        super("SearXNG");
        this.instanceUrl = instanceUrl;
    }

    async search(query) {
        try {
            const url = `${this.instanceUrl}/search?q=${encodeURIComponent(query)}&format=json`;
            const response = await fetch(url);
            const data = await response.json();

            if (!data.results) return [];

            return data.results.map(r => ({
                title: r.title,
                snippet: r.content || r.snippet,
                url: r.url,
                source: r.engine || "SearXNG"
            })).slice(0, 10);
        } catch (err) {
            console.error(`SearXNG error on ${this.instanceUrl}:`, err);
            // Fallback strategy could be implemented here if multiple instances are provided
            return [];
        }
    }
}

/**
 * Brave Search API Provider
 * Provides robust web results with a generous free tier.
 */
export class BraveSearchProvider extends SearchProvider {
    constructor(apiKey) {
        super("Brave");
        this.apiKey = apiKey;
    }

    async search(query) {
        if (!this.apiKey) return [];
        
        try {
            const url = `https://api.search.brave.com/res/v1/web/search?q=${encodeURIComponent(query)}`;
            const response = await fetch(url, {
                headers: { 'X-Subscription-Token': this.apiKey, 'Accept': 'application/json' }
            });

            if (!response.ok) return [];
            
            const data = await response.json();
            return (data.web?.results || []).map(r => ({
                title: r.title, snippet: r.description, url: r.url, source: "Brave"
            }));
        } catch (e) { return []; }
    }
}

/**
 * Tavily Search API Provider
 * Focuses on AI-optimized search results.
 */
export class TavilySearchProvider extends SearchProvider {
    constructor(apiKey) {
        super("Tavily");
        this.apiKey = apiKey;
    }

    async search(query) {
        if (!this.apiKey) return [];

        try {
            const url = `https://api.tavily.com/search`;
            const response = await fetch(url, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({
                    api_key: this.apiKey,
                    query: query,
                    search_depth: "basic",
                    include_answer: false,
                    max_results: 10
                })
            });

            if (!response.ok) return [];

            const data = await response.json();
            return (data.results || []).map(r => ({
                title: r.title,
                snippet: r.content,
                url: r.url,
                source: "Tavily"
            }));
        } catch (e) {
            console.error("Tavily search error:", e);
            return [];
        }
    }
}

/**
 * MultiProvider that tries providers in sequence until one returns results.
 */
export class MultiProvider extends SearchProvider {
    constructor(providers) {
        super("MultiProvider");
        this.providers = providers;
    }

    async search(query) {
        for (const provider of this.providers) {
            try {
                const results = await provider.search(query);
                if (results && results.length > 0) {
                    console.log(`Results found using ${provider.name}`);
                    return results;
                }
            } catch (err) {
                console.warn(`Provider ${provider.name} failed:`, err);
            }
        }
        return [];
    }
}
