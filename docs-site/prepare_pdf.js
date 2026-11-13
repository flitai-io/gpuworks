async ({ version, commit, language }) => {
  document.querySelector('[data-handbook-version]').textContent = version;
  document.querySelector('[data-handbook-commit]').textContent = commit;
  const articles = [...document.querySelectorAll('article[data-page-path]')];
  if (!articles.length) throw new Error('The handbook contains no articles');
  const canonical = new URL(document.querySelector('meta[name="handbook-base-url"]').content);
  const pages = new Map(articles.map(article => [decodeURI(article.dataset.pagePath), article]));
  const anchors = new Map();

  // IDs are page-local on the website. Compact ASCII destinations also stay
  // within PDF 1.4's name limit when a source heading has a long CJK title.
  for (const article of articles) {
    const mapping = new Map();
    for (const [index, element] of [...article.querySelectorAll('[id]')].entries()) {
      const id = `${article.id}--anchor-${index}`;
      mapping.set(element.id, id);
      element.id = id;
    }
    anchors.set(article, mapping);
  }
  for (const link of document.querySelectorAll('a[href]')) {
    const href = link.getAttribute('href');
    const article = link.closest('article');
    if (href.startsWith('#') && !article) continue;
    const origin = article ? new URL(article.dataset.pagePath, location.origin) : new URL(location.href);
    const target = new URL(href, origin);
    if (![location.origin, canonical.origin].includes(target.origin)) continue;
    const page = pages.get(decodeURI(target.pathname));
    if (page) {
      const id = target.hash ? anchors.get(page).get(decodeURIComponent(target.hash.slice(1))) : page.id;
      if (!id || !document.getElementById(id)) throw new Error(`Missing handbook anchor: ${href}`);
      link.setAttribute('href', `#${id}`);
    } else {
      target.protocol = canonical.protocol;
      target.host = canonical.host;
      target.port = canonical.port;
      link.href = target.href;
    }
  }
  for (const image of document.images) image.loading = 'eager';
  for (const image of document.images) {
    try { await image.decode(); }
    catch { throw new Error(`Cannot decode handbook image: ${image.currentSrc || image.src}`); }
  }
  await document.fonts.ready;

  const diagrams = [...document.querySelectorAll('.td-mermaid-source')];
  if (diagrams.length) {
    if (!window.mermaid) throw new Error('The local Mermaid renderer is unavailable');
    window.mermaid.initialize({ startOnLoad: false, securityLevel: 'strict', theme: 'default', fontFamily: 'Noto Sans CJK SC, sans-serif' });
    for (const [index, block] of diagrams.entries()) {
      const { svg } = await window.mermaid.render(`handbook-diagram-${index}`, block.textContent);
      const figure = document.createElement('figure');
      figure.innerHTML = svg;
      block.replaceWith(figure);
    }
  }
  for (const details of document.querySelectorAll('details')) details.open = true;
  await document.fonts.ready;
  const badLinks = [...document.querySelectorAll('a[href]')].filter(link => link.href.startsWith(location.origin) && !link.getAttribute('href').startsWith('#'));
  if (badLinks.length) throw new Error('The handbook contains temporary server links');
  const overflow = [...document.querySelectorAll('pre, table')].filter(element => element.scrollWidth > element.clientWidth + 2);
  if (overflow.length) throw new Error(`Handbook content exceeds the page width: ${overflow.length} blocks`);
  return { language, articles: articles.length, images: document.images.length, diagrams: diagrams.length,
    lastTitle: articles.at(-1).querySelector('h1').textContent };
}
