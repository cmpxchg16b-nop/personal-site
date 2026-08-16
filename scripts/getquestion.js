function getTitle(ele) {
  const cnodes = ele.querySelector(".card-body .card-text").childNodes;
  return cnodes[cnodes.length - 1].data.trim();
}

function isMultiChoice(ele) {
  return ele.querySelector(".text-info")?.textContent.trim() === "[多选]";
}

function isOptionCorrect(ele) {
  return ele.querySelector("span") === ele.querySelector("span.right");
}

function getOptionText(ele) {
  const ccnodes = ele.querySelector("label")?.childNodes ?? [];
  if (ccnodes.length === 0) {
    return "";
  }
  return ccnodes[ccnodes.length - 1]?.data.trim();
}

function getOptionEles(ele) {
  const cnodes = ele.querySelectorAll(".list-group-item");
  return cnodes;
}

function getOptions(ele) {
  const optionEles = getOptionEles(ele);
  const options = [];
  for (const ele of optionEles) {
    const opt = { isCorrect: isOptionCorrect(ele), text: getOptionText(ele) };
    options.push(opt);
  }
  return options;
}

function getQuestion(ele) {
  const title = getTitle(ele);
  const isMulti = isMultiChoice(ele);
  const opts = getOptions(ele);
  return { title, isMulti, opts };
}

function getQuestions(rowele) {
  const questionEles = rowele.querySelectorAll("div.card");
  const questions = [];
  for (const ele of questionEles) {
    const question = getQuestion(ele);
    questions.push(question);
  }
  return questions;
}

function getQuestionsFromPage() {
  const rowEles = window.document.querySelectorAll("div.container div.row");
  if (rowEles) {
    for (const rowEle of rowEles) {
      const questions = getQuestions(rowEle);
      if (questions && questions.length > 0) {
        return questions;
      }
    }
  }
  return [];
}

async function clickAll() {
  const nodes = document.querySelectorAll(
    ".card .list-group .list-group-item .btn"
  );
  for (const node of nodes) {
    await new Promise((res) => setTimeout(() => res(), 400));
    const clickEvent = new MouseEvent("click", {
      bubbles: true,
      cancelable: true,
      view: window,
    });
    node.dispatchEvent(clickEvent);
  }
}

await clickAll();

await new Promise((res) => setTimeout(() => res(), 1000));

getQuestionsFromPage()
  .map((question) => JSON.stringify(question))
  .join("\n");
